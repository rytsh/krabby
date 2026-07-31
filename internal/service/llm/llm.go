// Package llm is an OpenAI-compatible chat-completions client used by the doc
// generator and the config test endpoints.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/observability/langfuse"
)

// ErrNotConfigured is returned when the LLM has no base URL configured.
var ErrNotConfigured = errors.New("llm not configured (set llm.base_url)")

// Message is a single chat message.
type Message struct {
	Role    string        `json:"role"` // "system" | "user" | "assistant"
	Content string        `json:"content"`
	Parts   []ContentPart `json:"-"`
}

// ContentPart is one item in an OpenAI-compatible multimodal message.
type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL describes an image supplied to a multimodal model.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// TextPart builds a text content part.
func TextPart(text string) ContentPart {
	return ContentPart{Type: "text", Text: text}
}

// ImageURLPart builds an image_url content part.
func ImageURLPart(rawURL, detail string) ContentPart {
	return ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: rawURL, Detail: detail}}
}

// MarshalJSON preserves the original string content representation unless the
// caller explicitly supplies content parts.
func (m Message) MarshalJSON() ([]byte, error) {
	content := any(m.Content)
	if m.Parts != nil {
		content = m.Parts
	}

	return json.Marshal(struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{Role: m.Role, Content: content})
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

	// noStreamOpts latches when the endpoint has rejected the
	// stream_options parameter, so subsequent calls omit it instead of
	// paying a failed round trip each time. Same pattern as the embedder's
	// dimensions probe.
	noStreamOpts atomic.Bool

	// tracer exports each call as a Langfuse generation. Never nil.
	tracer *langfuse.Tracer
	// system is the gen_ai.system value derived once from the base URL.
	system string
}

// Option customizes a client at construction.
type Option func(*Client)

// WithTracer attaches an LLM-observability tracer. Passing nil is allowed and
// leaves the client untraced.
func WithTracer(t *langfuse.Tracer) Option {
	return func(c *Client) {
		if t != nil {
			c.tracer = t
		}
	}
}

// defaultIdleTimeout is the maximum time to wait for the next streamed chunk
// (or the initial response headers) before giving up. Generous enough for slow
// reasoning models to start, tight enough to detect a truly stalled endpoint.
const defaultIdleTimeout = 120 * time.Second

// New builds a chat client from config. Returns ErrNotConfigured when no base
// URL is set so callers can disable doc generation gracefully.
func New(cfg config.LLM, opts ...Option) (*Client, error) {
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

	c := &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		// No total timeout on the http.Client: streaming responses can be
		// long-lived. Liveness is enforced by the per-chunk idle timer.
		http:        &http.Client{},
		idleTimeout: idle,
		tracer:      langfuse.Disabled(),
		system:      langfuse.SystemFromBaseURL(cfg.BaseURL),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c, nil
}

// Model returns the configured chat model name.
func (c *Client) Model() string { return c.model }

// CacheIdentity identifies completion behavior without persisting the provider
// URL itself. Credentials are intentionally excluded.
func (c *Client) CacheIdentity() string {
	sum := sha256.Sum256([]byte(c.baseURL))
	return c.model + "@" + base64.RawURLEncoding.EncodeToString(sum[:12])
}

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Stream    bool      `json:"stream,omitempty"`
	// StreamOptions asks the provider to emit a final usage chunk. Omitted
	// entirely once noStreamOpts latches, because a few OpenAI-compatible
	// gateways reject the unknown field with a 400.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Usage carries the token accounting a provider reports for one call. Fields
// are zero when the provider does not report them.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Meta describes one completed chat call beyond its text: what the provider
// charged for it and how quickly it started producing output.
type Meta struct {
	Model string
	Usage Usage
	// FirstTokenAt is when the first content delta arrived. Zero when the
	// response was not streamed (gateway fallback path).
	FirstTokenAt time.Time
	// Attempts counts how many HTTP requests were made, including retries.
	Attempts int
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage *Usage    `json:"usage,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

// streamChunk is one server-sent event payload from a streaming chat call.
type streamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	// Usage is present only on the final chunk, and only when the provider
	// honoured stream_options.include_usage.
	Usage *Usage    `json:"usage,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Complete sends messages and returns the assistant's reply text.
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	text, _, err := c.CompleteOp(ctx, "chat", messages)

	return text, err
}

// CompleteOp is Complete with an operation name for the exported trace (e.g.
// "chat.summary"), and it returns the provider's token accounting alongside
// the text.
func (c *Client) CompleteOp(ctx context.Context, op string, messages []Message) (string, Meta, error) {
	if op == "" {
		op = "chat"
	}

	ctx, end := c.tracer.StartGeneration(ctx, langfuse.ScopeDocs, langfuse.GenerationInfo{
		Name:      op,
		System:    c.system,
		Operation: "chat",
		Model:     c.model,
		Input:     traceMessages(messages),
	})

	text, meta, err := c.complete(ctx, messages, 0)

	end(langfuse.GenerationResult{
		Output:        text,
		InputTokens:   meta.Usage.PromptTokens,
		OutputTokens:  meta.Usage.CompletionTokens,
		ResponseModel: meta.Model,
		FirstTokenAt:  meta.FirstTokenAt,
		Attempts:      meta.Attempts,
		Err:           err,
	})

	return text, meta, err
}

type traceMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type traceContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *traceImageURL `json:"image_url,omitempty"`
}

type traceImageURL struct {
	Source string `json:"source"`
	Detail string `json:"detail,omitempty"`
	MIME   string `json:"mime,omitempty"`
	Bytes  *int64 `json:"bytes,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// traceMessages creates the representation passed to observability. Image URLs
// are never copied into it: remote query credentials stay private, and data URL
// payloads are represented only by safe metadata.
func traceMessages(messages []Message) []traceMessage {
	out := make([]traceMessage, len(messages))
	for i, message := range messages {
		out[i] = traceMessage{Role: message.Role, Content: message.Content}
		if message.Parts == nil {
			continue
		}

		parts := make([]traceContentPart, len(message.Parts))
		for j, part := range message.Parts {
			parts[j] = traceContentPart{Type: part.Type}
			if part.Type == "text" {
				parts[j].Text = part.Text
			}
			if part.ImageURL != nil {
				parts[j].ImageURL = redactImageURL(part.ImageURL)
			}
		}
		out[i].Content = parts
	}

	return out
}

func redactImageURL(image *ImageURL) *traceImageURL {
	redacted := &traceImageURL{Source: "remote_url", Detail: image.Detail}
	if len(image.URL) < 5 || !strings.EqualFold(image.URL[:5], "data:") {
		return redacted
	}

	redacted.Source = "data_url"
	metadata, payload, ok := strings.Cut(image.URL[5:], ",")
	if !ok {
		return redacted
	}

	mediaType, _, _ := strings.Cut(metadata, ";")
	if mediaType == "" {
		mediaType = "text/plain"
	}
	redacted.MIME = mediaType

	var reader io.Reader
	if hasDataURLFlag(metadata, "base64") {
		reader = base64.NewDecoder(base64.StdEncoding, strings.NewReader(payload))
	} else {
		decoded, err := url.PathUnescape(payload)
		if err != nil {
			return redacted
		}
		reader = strings.NewReader(decoded)
	}

	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		return redacted
	}
	redacted.Bytes = &size
	redacted.SHA256 = fmt.Sprintf("%x", hash.Sum(nil))

	return redacted
}

func hasDataURLFlag(metadata, flag string) bool {
	for field := range strings.SplitSeq(metadata, ";") {
		if strings.EqualFold(field, flag) {
			return true
		}
	}

	return false
}

// maxAttempts bounds retries for transient failures (429, 5xx, network).
const maxAttempts = 3

func (c *Client) complete(ctx context.Context, messages []Message, maxTokens int) (string, Meta, error) {
	meta := Meta{Model: c.model}

	body, err := c.marshalRequest(messages, maxTokens)
	if err != nil {
		return "", meta, err
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		meta.Attempts = attempt

		res, retryAfter, retryable, err := c.completeOnce(ctx, body)
		if err == nil {
			meta.Usage = res.usage
			meta.FirstTokenAt = res.firstTokenAt
			if res.model != "" {
				meta.Model = res.model
			}

			return res.text, meta, nil
		}

		lastErr = err

		// A gateway that does not understand stream_options answers 400 on
		// every attempt, so retrying the same body is pointless. Drop the
		// field once and replay immediately rather than burning a backoff.
		if errors.Is(err, errStreamOptsRejected) && c.noStreamOpts.CompareAndSwap(false, true) {
			slog.Warn("endpoint rejected stream_options; retrying without token usage reporting",
				"model", c.model)

			retry, merr := c.marshalRequest(messages, maxTokens)
			if merr != nil {
				return "", meta, merr
			}

			body = retry

			continue
		}

		if !retryable || attempt >= maxAttempts || ctx.Err() != nil {
			return "", meta, lastErr
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
			return "", meta, lastErr
		}
	}
}

// marshalRequest builds the request body, including stream_options unless the
// endpoint has already rejected it.
func (c *Client) marshalRequest(messages []Message, maxTokens int) ([]byte, error) {
	req := chatRequest{Model: c.model, Messages: messages, MaxTokens: maxTokens, Stream: true}
	if !c.noStreamOpts.Load() {
		req.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request; %w", err)
	}

	return body, nil
}

// completion is the outcome of one successful streaming chat call.
type completion struct {
	text  string
	model string
	usage Usage
	// firstTokenAt is when the first content delta arrived; zero when the
	// gateway returned a non-streamed body.
	firstTokenAt time.Time
}

// completeOnce performs a single streaming chat call. retryable reports whether
// the failure is transient (rate limit, server error, network hiccup); an idle
// timeout (the server stopped producing chunks) and caller cancellation are not
// retried because repeating them would just double the wall time.
//
// The response is consumed as OpenAI-style server-sent events. A per-chunk idle
// timer bounds liveness: as long as chunks keep arriving the call may run for
// minutes, but a stalled endpoint is aborted after idleTimeout.
func (c *Client) completeOnce(ctx context.Context, body []byte) (res completion, retryAfter time.Duration, retryable bool, err error) {
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
		return completion{}, 0, false, fmt.Errorf("new chat request; %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if idledOut.Load() {
			return completion{}, 0, false, fmt.Errorf("chat request stalled: no response within %s", c.idleTimeout)
		}
		var netErr interface{ Timeout() bool }
		timedOut := errors.As(err, &netErr) && netErr.Timeout()

		return completion{}, 0, !timedOut && ctx.Err() == nil, fmt.Errorf("chat request; %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		httpErr := fmt.Errorf("chat http %d; %s", resp.StatusCode, apiErrMsg(raw))

		// Some OpenAI-compatible gateways reject the unknown stream_options
		// field outright. Flag it so the caller can replay without it instead
		// of disabling the whole call path.
		if rejectsStreamOptions(resp.StatusCode, raw) {
			httpErr = fmt.Errorf("%w; %w", errStreamOptsRejected, httpErr)
		}

		return completion{}, parseRetryAfter(resp.Header.Get("Retry-After")), retryable, httpErr
	}

	res, err = c.readStream(resp.Body, resetIdle)
	if err != nil {
		if idledOut.Load() {
			return completion{}, 0, false, fmt.Errorf("chat stream stalled: no data within %s", c.idleTimeout)
		}
		if ctx.Err() != nil {
			return completion{}, 0, false, ctx.Err()
		}
		// An error explicitly reported by the API in the stream is not
		// transient; only genuine transport read failures are retried.
		if errors.Is(err, errAPIStream) {
			return completion{}, 0, false, err
		}

		return completion{}, 0, true, err // mid-stream read failure: transient, retry
	}

	return res, 0, false, nil
}

// errAPIStream marks an error the model/gateway reported inside the SSE stream,
// so the retry logic can treat it as terminal rather than transient.
var errAPIStream = errors.New("chat api stream error")

// errStreamOptsRejected marks a 400 that names stream_options, so the caller
// can replay the request without it. Usage reporting is best-effort: losing it
// must never lose the completion itself.
var errStreamOptsRejected = errors.New("endpoint rejected stream_options")

// rejectsStreamOptions reports whether a non-2xx response is the gateway
// complaining specifically about stream_options. Matching on the body keeps a
// generic 400 (bad model, oversized prompt) from silently disabling usage
// reporting for the process lifetime.
func rejectsStreamOptions(status int, raw []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}

	return strings.Contains(strings.ToLower(string(raw)), "stream_options")
}

// readStream parses an OpenAI SSE chat stream into the concatenated assistant
// text, calling onProgress after each line so the caller can reset its idle
// timer. If the stream yields no SSE data events (some gateways ignore
// stream:true and return a normal JSON body), it falls back to decoding the
// whole buffered body as a single chat response.
func (c *Client) readStream(body io.Reader, onProgress func()) (completion, error) {
	sc := bufio.NewScanner(body)
	// Allow long single-line data payloads (big deltas / fallback whole body).
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)

	var (
		out     strings.Builder
		res     completion
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
			return completion{}, fmt.Errorf("%w; %s", errAPIStream, chunk.Error.Message)
		}

		if chunk.Model != "" && res.model == "" {
			res.model = chunk.Model
		}
		// The usage chunk arrives last and carries no choices; keep the last
		// non-nil one so a provider that repeats it does not lose the totals.
		if chunk.Usage != nil {
			res.usage = *chunk.Usage
		}

		for _, ch := range chunk.Choices {
			if ch.Delta.Content == "" {
				continue
			}
			// Time to first token: measured at the first chunk that actually
			// carries content, not at the role-only preamble chunk.
			if res.firstTokenAt.IsZero() {
				res.firstTokenAt = time.Now()
			}

			out.WriteString(ch.Delta.Content)
		}

		sawData = true
	}

	if err := sc.Err(); err != nil {
		return completion{}, fmt.Errorf("read chat stream; %w", err)
	}

	if sawData {
		res.text = out.String()

		return res, nil
	}

	// Fallback: decode the whole body as a non-streamed chat response.
	var whole chatResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(allRaw.String())), &whole); err == nil {
		if whole.Error != nil {
			return completion{}, fmt.Errorf("%w; %s", errAPIStream, whole.Error.Message)
		}
		if len(whole.Choices) > 0 {
			if whole.Usage != nil {
				res.usage = *whole.Usage
			}

			res.model = whole.Model
			res.text = whole.Choices[0].Message.Content

			return res, nil
		}
	}

	return completion{}, errors.New("chat response had no choices")
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
	_, _, err := c.complete(ctx, []Message{{Role: "user", Content: "ping"}}, 1)

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
