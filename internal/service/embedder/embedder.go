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
	"sync/atomic"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

// ErrNotConfigured is returned when no embeddings base URL is configured.
var ErrNotConfigured = errors.New("embedder not configured (set embedder.base_url)")

// maxSafeBatch is the largest per-request input count sent to the embeddings
// endpoint regardless of the configured batch, chosen to satisfy the most
// restrictive common provider limit (Google Gemini caps a batch at 100).
const maxSafeBatch = 100

// maxRespBytes bounds a single embeddings response body so a misbehaving
// endpoint cannot exhaust memory. It has to clear the widest legitimate
// response by a wide margin: a full batch of 100 vectors at 3072 dimensions
// (Gemini Embedding 2's default width) is several MiB of JSON floats, and a
// body truncated at the limit fails as an opaque JSON decode error rather than
// as the size problem it is — so the limit is generous and overruns are
// reported explicitly.
const maxRespBytes = 32 << 20

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
	outDim  int
	batch   int
	conc    int
	http    *http.Client

	// noDims latches when the endpoint has rejected the "dimensions"
	// parameter, so the remaining requests of a run stop sending it.
	noDims atomic.Bool

	dimMu     sync.Mutex
	dim       int
	dimWarned bool
}

// New builds an embeddings client from config. Returns ErrNotConfigured when no
// base URL is set so RAG can be disabled gracefully.
//
// A non-zero cfg.Dim is requested from the provider (the OpenAI "dimensions"
// parameter) rather than merely asserted locally. Models trained with
// Matryoshka Representation Learning — Gemini Embedding 2 (128-3072),
// text-embedding-3 — keep most of their accuracy at a fraction of their default
// width, and the width decides the one budget GOMEMLIMIT cannot enforce: the
// decoded vectors bw holds to traverse the HNSW graph. Endpoints that do not
// understand the parameter are detected on first use and fall back to their
// native width (see embedBatch).
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
		outDim:  cfg.Dim,
		dim:     cfg.Dim,
		batch:   batch,
		conc:    conc,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// Model returns the configured embedding model name.
func (c *Client) Model() string { return c.model }

// Dim returns the embedding dimension: the configured one until the first
// response, and the width actually returned from then on.
func (c *Client) Dim() int {
	c.dimMu.Lock()
	defer c.dimMu.Unlock()

	return c.dim
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
	// Dimensions truncates the output vector on providers that support it
	// (OpenAI text-embedding-3, Gemini via its OpenAI-compatible layer, where
	// it maps to output_dimensionality). Omitted when zero.
	Dimensions int `json:"dimensions,omitempty"`
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
//
// One non-retryable case is retried anyway, once per client: a configured
// output dimension rejected by the endpoint. Only some providers accept the
// "dimensions" parameter, and a local Ollama/TEI/vLLM deployment that does not
// would otherwise fail every request the moment a dimension is configured.
// The parameter is dropped for the rest of the client's life and the request
// is replayed; if it still fails, the real error surfaces.
func (c *Client) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt <= maxEmbedRetries; attempt++ {
		sentDims := c.requestDim()

		vecs, retryAfter, err := c.embedBatchOnce(ctx, batch, sentDims)
		if err == nil {
			return vecs, nil
		}

		var re retryableErr
		if !errors.As(err, &re) {
			// Every in-flight batch that carried the parameter retries, not
			// just the one that latched the flag: batches run concurrently, so
			// the others are failing for the same reason at the same moment
			// and would otherwise abort the run. The replay carries no
			// dimension, so this cannot loop.
			if sentDims > 0 {
				if c.noDims.CompareAndSwap(false, true) {
					slog.Warn("embeddings endpoint rejected the requested output dimension; retrying at the model's native width",
						"model", c.model, "dim", sentDims, "error", err)
				}

				// Keep the error in case this was the last attempt: the
				// replay would otherwise exit the loop empty-handed.
				lastErr = err

				continue
			}

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

// requestDim is the output dimension to ask for, or 0 when none is configured
// or the endpoint has already rejected the parameter.
func (c *Client) requestDim() int {
	if c.outDim <= 0 || c.noDims.Load() {
		return 0
	}

	return c.outDim
}

// embedBatchOnce performs a single embeddings request. On a retryable failure
// it returns a retryableErr and, when the server advertised one, a retry delay.
func (c *Client) embedBatchOnce(ctx context.Context, batch []string, dims int) ([][]float32, time.Duration, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: batch, Dimensions: dims})
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

	// Read one byte past the limit so an overrun is distinguishable from a
	// body that merely ends there.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))
	if len(raw) > maxRespBytes {
		return nil, 0, fmt.Errorf("embed response exceeds %d bytes for %d inputs; lower the batch size or the embedding dimension", maxRespBytes, len(batch))
	}

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
		c.observeDim(len(vecs[0]))
	}

	return vecs, 0, nil
}

// observeDim records the width the endpoint actually returned. The response is
// authoritative over the configured dimension: a provider that ignores the
// "dimensions" parameter rather than rejecting it (several local servers accept
// and discard unknown fields) would otherwise leave the client reporting a
// width the vector store never sees, since the store locks in the dimension of
// the vectors it is handed. The disagreement is worth one warning: it means the
// memory saving the configured dimension was meant to buy is not happening.
func (c *Client) observeDim(got int) {
	c.dimMu.Lock()
	defer c.dimMu.Unlock()

	if c.dim != got && !c.dimWarned {
		c.dimWarned = true

		if c.outDim > 0 && !c.noDims.Load() {
			slog.Warn("embeddings endpoint ignored the requested output dimension",
				"model", c.model, "requested", c.outDim, "got", got)
		}
	}

	c.dim = got
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
