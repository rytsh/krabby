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
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

// ErrNotConfigured is returned when no embeddings base URL is configured.
var ErrNotConfigured = errors.New("embedder not configured (set embedder.base_url)")

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
	if len(texts) == 0 {
		return nil, nil
	}

	if len(texts) <= c.batch {
		return c.embedBatch(ctx, texts)
	}

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make([][]float32, len(texts))
	sem := make(chan struct{}, c.conc)

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)

	for start := 0; start < len(texts) && ctx.Err() == nil; start += c.batch {
		end := min(start+c.batch, len(texts))

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

func (c *Client) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: batch})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request; %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new embed request; %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request; %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embed http %d; %s", resp.StatusCode, apiErrMsg(raw))
	}

	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode embed response; %w", err)
	}

	if out.Error != nil {
		return nil, fmt.Errorf("embed api error; %s", out.Error.Message)
	}

	if len(out.Data) != len(batch) {
		return nil, fmt.Errorf("embed response count mismatch: got %d for %d inputs", len(out.Data), len(batch))
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

	return vecs, nil
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
