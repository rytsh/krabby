// Package embedder is an OpenAI-compatible embeddings client. It converts text
// into vectors for the RAG index. Works with OpenAI, Ollama, LM Studio, TEI and
// vLLM endpoints that expose /v1/embeddings.
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

// ErrNotConfigured is returned when no embeddings base URL is configured.
var ErrNotConfigured = errors.New("embedder not configured (set embedder.base_url)")

// maxSafeBatch is the largest per-request input count sent to the embeddings
// endpoint regardless of the configured batch, chosen to satisfy the most
// restrictive common provider limit (Google Gemini caps a batch at 100).
const maxSafeBatch = 100

// Retry/backoff tuning for transient embed failures (HTTP 429 rate limits and
// 5xx). Large indexing runs (tens of thousands of chunks) routinely trip a
// provider's per-minute quota; without retry a single 429 aborts the whole run
// and no vectors are written. Retries use exponential backoff with jitter and
// honour a server-provided Retry-After / "retry in Ns" hint when present.
const (
	maxEmbedRetries  = 6
	baseEmbedBackoff = 2 * time.Second
	maxEmbedBackoff  = 60 * time.Second
)

// Client talks to an OpenAI-compatible /embeddings endpoint.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	batch   int
	conc    int
	http    *http.Client

	dimMu sync.Mutex
	dim   int
}

// New builds an embeddings client from config. Returns ErrNotConfigured when no
// base URL is set so RAG can be disabled gracefully.
func New(cfg config.Embedder) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, ErrNotConfigured
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	batch := cfg.Batch
	if batch <= 0 {
		batch = 64
	}
	// Cap the per-request batch at a provider-safe ceiling. Several embedding
	// backends reject oversized batches outright rather than truncating them
	// (Google Gemini: "at most 100 requests can be in one batch", HTTP 400),
	// which fails every embed call and silently breaks indexing. 100 is the
	// most restrictive common limit; OpenAI/TEI/vLLM accept far more but run
	// fine at 100, and lost throughput is recovered via Concurrency. A larger
	// configured value is clamped instead of trusted.
	if batch > maxSafeBatch {
		batch = maxSafeBatch
	}

	conc := cfg.Concurrency
	if conc <= 0 {
		conc = 4
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		dim:     cfg.Dim,
		batch:   batch,
		conc:    conc,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// Model returns the configured embedding model name.
func (c *Client) Model() string { return c.model }

// Dim returns the embedding dimension (0 until inferred from a response).
func (c *Client) Dim() int {
	c.dimMu.Lock()
	defer c.dimMu.Unlock()

	return c.dim
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Embed returns one vector per input text, batching requests by the configured
// batch size and preserving input order. Batches are dispatched concurrently
// (bounded by the configured concurrency); the first failure cancels the rest.
// On the first successful response it records the embedding dimension when not
// already set.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return c.EmbedWithProgress(ctx, texts, nil)
}

// EmbedWithProgress is Embed with an optional callback invoked after each batch
// completes, reporting how many inputs have been embedded so far out of the
// total. The callback runs from multiple goroutines, so it must be safe for
// concurrent use; pass nil to disable progress reporting. It exists so callers
// (e.g. indexing a large web source) can drive a determinate progress bar.
func (c *Client) EmbedWithProgress(ctx context.Context, texts []string, onProgress func(done, total int)) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	total := len(texts)

	if total <= c.batch {
		vecs, err := c.embedBatch(ctx, texts)
		if err == nil && onProgress != nil {
			onProgress(total, total)
		}

		return vecs, err
	}

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make([][]float32, total)
	sem := make(chan struct{}, c.conc)

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
		doneMu   sync.Mutex
		done     int
	)

	for start := 0; start < total && ctx.Err() == nil; start += c.batch {
		end := min(start+c.batch, total)

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			continue
		}

		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			defer func() { <-sem }()

			vecs, err := c.embedBatch(ctx, texts[start:end])
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()

				cancel()

				return
			}

			copy(out[start:end], vecs)

			if onProgress != nil {
				doneMu.Lock()
				done += end - start
				at := done
				doneMu.Unlock()
				onProgress(at, total)
			}
		}(start, end)
	}

	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	if err := parent.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// embedBatch sends one batch, retrying transient failures (HTTP 429 and 5xx)
// with exponential backoff and jitter so a per-minute provider quota does not
// abort a large indexing run. A non-retryable error or a cancelled context
// returns immediately.
func (c *Client) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt <= maxEmbedRetries; attempt++ {
		vecs, retryAfter, err := c.embedBatchOnce(ctx, batch)
		if err == nil {
			return vecs, nil
		}

		var re retryableErr
		if !errors.As(err, &re) {
			return nil, err // non-retryable: fail fast
		}
		lastErr = err

		if attempt == maxEmbedRetries {
			break
		}

		wait := backoffDelay(attempt, retryAfter)
		slog.Warn("embed batch transient failure; retrying",
			"attempt", attempt+1, "max", maxEmbedRetries, "wait", wait, "error", err)

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, fmt.Errorf("embed batch failed after %d retries; %w", maxEmbedRetries, lastErr)
}

// retryableErr marks a transient embed failure worth retrying.
type retryableErr struct{ err error }

func (e retryableErr) Error() string { return e.err.Error() }
func (e retryableErr) Unwrap() error { return e.err }

// backoffDelay computes the wait before the next attempt: it honours a
// server-provided retry hint when present, otherwise uses exponential backoff
// with full jitter, capped at maxEmbedBackoff.
func backoffDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxEmbedBackoff {
			return maxEmbedBackoff
		}

		return retryAfter
	}

	backoff := baseEmbedBackoff << attempt
	if backoff > maxEmbedBackoff {
		backoff = maxEmbedBackoff
	}

	return time.Duration(rand.Int63n(int64(backoff)) + int64(backoff)/2) //nolint:gosec // jitter, not security-sensitive
}

// embedBatchOnce performs a single embeddings request. On a retryable failure
// it returns a retryableErr and, when the server advertised one, a retry delay.
func (c *Client) embedBatchOnce(ctx context.Context, batch []string) ([][]float32, time.Duration, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: batch})
	if err != nil {
		return nil, 0, fmt.Errorf("marshal embed request; %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("new embed request; %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Network/transport errors are transient (unless the context was
		// cancelled): retry them.
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}

		return nil, 0, retryableErr{fmt.Errorf("embed request; %w", err)}
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httpErr := fmt.Errorf("embed http %d; %s", resp.StatusCode, apiErrMsg(raw))
		if isRetryableStatus(resp.StatusCode) {
			return nil, retryAfterHint(resp, raw), retryableErr{httpErr}
		}

		return nil, 0, httpErr
	}

	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, fmt.Errorf("decode embed response; %w", err)
	}

	if out.Error != nil {
		return nil, 0, fmt.Errorf("embed api error; %s", out.Error.Message)
	}

	if len(out.Data) != len(batch) {
		return nil, 0, fmt.Errorf("embed response count mismatch: got %d for %d inputs", len(out.Data), len(batch))
	}

	vecs := make([][]float32, len(out.Data))
	for i := range out.Data {
		vecs[i] = out.Data[i].Embedding
	}

	if len(vecs) > 0 {
		c.dimMu.Lock()
		if c.dim == 0 {
			c.dim = len(vecs[0])
		}
		c.dimMu.Unlock()
	}

	return vecs, 0, nil
}

// isRetryableStatus reports whether an HTTP status warrants a retry: 429 (rate
// limit) and any 5xx (transient server/gateway error, e.g. the 502/503 seen
// when the gateway surfaces an upstream 429).
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// retryAfterHint extracts a retry delay from the response: the standard
// Retry-After header (seconds) first, then a provider "retry in 10.6s" / "retry
// after 10s" phrase in the error body. Zero means no hint was found.
func retryAfterHint(resp *http.Response, raw []byte) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}

	return parseRetryPhrase(string(raw))
}

// parseRetryPhrase finds a "retry in <n>s" / "retry after <n>s" hint (as Gemini
// returns in its 429 body) and returns it rounded up to whole seconds.
func parseRetryPhrase(body string) time.Duration {
	lower := strings.ToLower(body)
	for _, marker := range []string{"retry in ", "retry after ", "retrydelay\": \""} {
		idx := strings.Index(lower, marker)
		if idx < 0 {
			continue
		}
		rest := lower[idx+len(marker):]
		num := strings.Builder{}
		for _, r := range rest {
			if (r >= '0' && r <= '9') || r == '.' {
				num.WriteRune(r)

				continue
			}
			break
		}
		if secs, err := strconv.ParseFloat(num.String(), 64); err == nil && secs > 0 {
			// Round up so we wait at least as long as advertised.
			return time.Duration((secs + 0.999) * float64(time.Second))
		}
	}

	return 0
}

// Ping embeds a single short string to validate the endpoint, credentials and
// model. On success the client's dimension is populated.
func (c *Client) Ping(ctx context.Context) error {
	vecs, err := c.embedBatch(ctx, []string{"ping"})
	if err != nil {
		return err
	}

	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return errors.New("embedder returned an empty vector")
	}

	return nil
}

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
