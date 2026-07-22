// Package llm is an OpenAI-compatible chat-completions client used by the doc
// generator and the config test endpoints.
package llm

import (
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
}

// New builds a chat client from config. Returns ErrNotConfigured when no base
// URL is set so callers can disable doc generation gracefully.
func New(cfg config.LLM) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, ErrNotConfigured
	}

	// Docs synthesis calls can legitimately run for minutes on large repos or
	// reasoning models; a short client timeout kills work the server is still
	// doing. 60s proved too tight in practice.
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 300 * time.Second
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// Model returns the configured chat model name.
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
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
	body, err := json.Marshal(chatRequest{Model: c.model, Messages: messages, MaxTokens: maxTokens})
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

// completeOnce performs a single chat call. retryable reports whether the
// failure is transient (rate limit, server error, network hiccup); client-side
// timeouts and cancellations are not retried because repeating them would just
// double the wall time.
func (c *Client) completeOnce(ctx context.Context, body []byte) (text string, retryAfter time.Duration, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, false, fmt.Errorf("new chat request; %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		var netErr interface{ Timeout() bool }
		timedOut := errors.As(err, &netErr) && netErr.Timeout()

		return "", 0, !timedOut && ctx.Err() == nil, fmt.Errorf("chat request; %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

		return "", parseRetryAfter(resp.Header.Get("Retry-After")), retryable,
			fmt.Errorf("chat http %d; %s", resp.StatusCode, apiErrMsg(raw))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, false, fmt.Errorf("decode chat response; %w", err)
	}

	if out.Error != nil {
		return "", 0, false, fmt.Errorf("chat api error; %s", out.Error.Message)
	}

	if len(out.Choices) == 0 {
		return "", 0, false, errors.New("chat response had no choices")
	}

	return out.Choices[0].Message.Content, 0, false, nil
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
