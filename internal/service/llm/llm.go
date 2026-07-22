// Package llm is an OpenAI-compatible chat-completions client used by the doc
// generator and the config test endpoints.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

// ErrNotConfigured is returned when the LLM has no base URL configured.
var ErrNotConfigured = errors.New("llm not configured (set llm.base_url)")

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Client talks to an OpenAI-compatible /chat/completions endpoint.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client

	// idleTimeout bounds the gap between streamed chunks rather than the
	// total call duration, so a long-but-progressing generation is never
	// killed while it is still producing tokens.
	idleTimeout time.Duration
}

// defaultIdleTimeout is the maximum time to wait for the next streamed chunk
// (or the initial response headers) before giving up. Generous enough for slow
// reasoning models to start, tight enough to detect a truly stalled endpoint.
const defaultIdleTimeout = 120 * time.Second

// New builds a chat client from config. Returns ErrNotConfigured when no base
// URL is set so callers can disable doc generation gracefully.
func New(cfg config.LLM) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, ErrNotConfigured
	}

	// Docs synthesis calls can legitimately run for minutes on large repos or
	// reasoning models. Rather than a total wall-clock timeout (which kills
	// work the server is still streaming), we stream the response and bound
	// the idle gap between chunks. cfg.Timeout, when set, is treated as that
	// idle timeout; otherwise a sensible default is used.
	// Treat cfg.Timeout as the idle gap, but never below the default: a
	// reasoning model can take longer than a small configured value just to
	// emit its first token, and idle time is far cheaper than total time.
	idle := cfg.Timeout
	if idle < defaultIdleTimeout {
		idle = defaultIdleTimeout
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		// No total timeout on the http.Client: streaming responses can be
		// long-lived. Liveness is enforced by the per-chunk idle timer.
		http:        &http.Client{},
		idleTimeout: idle,
	}, nil
}

// Model returns the configured chat model name.
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Stream    bool      `json:"stream,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *apiError `json:"error,omitempty"`
}

// streamChunk is one server-sent event payload from a streaming chat call.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Complete sends messages and returns the assistant's reply text.
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	return c.complete(ctx, messages, 0)
}

// maxAttempts bounds retries for transient failures (429, 5xx, network).
const maxAttempts = 3

func (c *Client) complete(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	body, err := json.Marshal(chatRequest{Model: c.model, Messages: messages, MaxTokens: maxTokens, Stream: true})
	if err != nil {
		return "", fmt.Errorf("marshal chat request; %w", err)
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		text, retryAfter, retryable, err := c.completeOnce(ctx, body)
		if err == nil {
			return text, nil
		}

		lastErr = err
		if !retryable || attempt >= maxAttempts || ctx.Err() != nil {
			return "", lastErr
		}

		delay := time.Duration(attempt) * 2 * time.Second
		if retryAfter > delay {
			delay = retryAfter
		}

		slog.Warn("chat request failed, retrying",
			"attempt", attempt, "max", maxAttempts, "delay", delay.String(), "error", err)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", lastErr
		}
	}
}

// completeOnce performs a single streaming chat call. retryable reports whether
// the failure is transient (rate limit, server error, network hiccup); an idle
// timeout (the server stopped producing chunks) and caller cancellation are not
// retried because repeating them would just double the wall time.
//
// The response is consumed as OpenAI-style server-sent events. A per-chunk idle
// timer bounds liveness: as long as chunks keep arriving the call may run for
// minutes, but a stalled endpoint is aborted after idleTimeout.
func (c *Client) completeOnce(ctx context.Context, body []byte) (text string, retryAfter time.Duration, retryable bool, err error) {
	// Derive a cancellable context and arm an idle timer that cancels it if
	// no progress is made for idleTimeout. Each received chunk resets it.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var idledOut atomic.Bool
	timer := time.AfterFunc(c.idleTimeout, func() {
		idledOut.Store(true)
		cancel()
	})
	defer timer.Stop()
	resetIdle := func() { timer.Reset(c.idleTimeout) }

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, false, fmt.Errorf("new chat request; %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if idledOut.Load() {
			return "", 0, false, fmt.Errorf("chat request stalled: no response within %s", c.idleTimeout)
		}
		var netErr interface{ Timeout() bool }
		timedOut := errors.As(err, &netErr) && netErr.Timeout()

		return "", 0, !timedOut && ctx.Err() == nil, fmt.Errorf("chat request; %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

		return "", parseRetryAfter(resp.Header.Get("Retry-After")), retryable,
			fmt.Errorf("chat http %d; %s", resp.StatusCode, apiErrMsg(raw))
	}

	text, err = c.readStream(resp.Body, resetIdle)
	if err != nil {
		if idledOut.Load() {
			return "", 0, false, fmt.Errorf("chat stream stalled: no data within %s", c.idleTimeout)
		}
		if ctx.Err() != nil {
			return "", 0, false, ctx.Err()
		}
		// An error explicitly reported by the API in the stream is not
		// transient; only genuine transport read failures are retried.
		if errors.Is(err, errAPIStream) {
			return "", 0, false, err
		}

		return "", 0, true, err // mid-stream read failure: transient, retry
	}

	return text, 0, false, nil
}

// errAPIStream marks an error the model/gateway reported inside the SSE stream,
// so the retry logic can treat it as terminal rather than transient.
var errAPIStream = errors.New("chat api stream error")

// readStream parses an OpenAI SSE chat stream into the concatenated assistant
// text, calling onProgress after each line so the caller can reset its idle
// timer. If the stream yields no SSE data events (some gateways ignore
// stream:true and return a normal JSON body), it falls back to decoding the
// whole buffered body as a single chat response.
func (c *Client) readStream(body io.Reader, onProgress func()) (string, error) {
	sc := bufio.NewScanner(body)
	// Allow long single-line data payloads (big deltas / fallback whole body).
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)

	var (
		out     strings.Builder
		sawData bool
		allRaw  strings.Builder // captured for the non-streaming fallback
	)

	for sc.Scan() {
		onProgress()
		raw := sc.Text()
		allRaw.WriteString(raw)
		allRaw.WriteByte('\n')

		line := strings.TrimSpace(raw)
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // ignore keep-alives / comments
		}
		if chunk.Error != nil {
			return "", fmt.Errorf("%w; %s", errAPIStream, chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			out.WriteString(ch.Delta.Content)
		}
		sawData = true
	}

	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read chat stream; %w", err)
	}

	if sawData {
		return out.String(), nil
	}

	// Fallback: decode the whole body as a non-streamed chat response.
	var whole chatResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(allRaw.String())), &whole); err == nil {
		if whole.Error != nil {
			return "", fmt.Errorf("%w; %s", errAPIStream, whole.Error.Message)
		}
		if len(whole.Choices) > 0 {
			return whole.Choices[0].Message.Content, nil
		}
	}

	return "", errors.New("chat response had no choices")
}

// parseRetryAfter reads a Retry-After header in seconds form; 0 when absent or
// unparseable (HTTP-date form is rare on LLM APIs and safely ignored).
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}

	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}

	// Cap so a hostile/buggy header cannot stall the pipeline for long.
	if secs > 60 {
		secs = 60
	}

	return time.Duration(secs) * time.Second
}

// Ping performs a minimal completion to validate the endpoint, credentials and
// model. It returns the model that answered (echoed from config) and any error.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.complete(ctx, []Message{{Role: "user", Content: "ping"}}, 1)

	return err
}

// apiErrMsg extracts a human-readable error message from an OpenAI-style error
// body, falling back to the raw payload.
func apiErrMsg(raw []byte) string {
	var out struct {
		Error *apiError `json:"error"`
	}

	if err := json.Unmarshal(raw, &out); err == nil && out.Error != nil && out.Error.Message != "" {
		return out.Error.Message
	}

	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "..."
	}

	return s
}
