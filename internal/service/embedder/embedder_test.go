package embedder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rytsh/krabby/internal/config"
)

func TestNewNotConfigured(t *testing.T) {
	if _, err := New(config.Embedder{}); err == nil {
		t.Fatal("expected ErrNotConfigured for empty base url")
	}
}

func embedServer(t *testing.T, vecs [][]float32) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q", r.URL.Path)
		}

		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		var resp embedResponse
		for i := range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: vecs[i%len(vecs)]})
		}

		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestEmbedBatchingAndDim(t *testing.T) {
	srv := embedServer(t, [][]float32{{0.1, 0.2, 0.3}})
	defer srv.Close()

	c, err := New(config.Embedder{BaseURL: srv.URL, Model: "m", Batch: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := c.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(out) != 3 {
		t.Fatalf("got %d vectors want 3", len(out))
	}

	if c.Dim() != 3 {
		t.Fatalf("dim = %d want 3", c.Dim())
	}
}

func TestEmbedClampsBatchToSafeMax(t *testing.T) {
	// Batches are dispatched concurrently, so the handler runs from several
	// goroutines and the high-water mark needs guarding.
	var (
		mu      sync.Mutex
		maxSeen int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		if len(req.Input) > maxSeen {
			maxSeen = len(req.Input)
		}
		mu.Unlock()

		var resp embedResponse
		for range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: []float32{1}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Configure an oversized batch; the client must clamp it so no single
	// request exceeds the provider-safe ceiling (Gemini rejects > 100).
	c, err := New(config.Embedder{BaseURL: srv.URL, Model: "m", Batch: 500})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inputs := make([]string, 250)
	for i := range inputs {
		inputs[i] = "x"
	}

	out, err := c.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != len(inputs) {
		t.Fatalf("got %d vectors want %d", len(out), len(inputs))
	}
	mu.Lock()
	defer mu.Unlock()
	if maxSeen > maxSafeBatch {
		t.Fatalf("largest request batch = %d, want <= %d", maxSeen, maxSafeBatch)
	}
}

// TestEmbedRequestsConfiguredDimension pins the point of a configured dim: it
// has to reach the provider, because that is the only way a Matryoshka model
// returns a narrower vector.
func TestEmbedRequestsConfiguredDimension(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		seen = append(seen, req.Dimensions)
		mu.Unlock()

		vec := make([]float32, req.Dimensions)

		var resp embedResponse
		for range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: vec})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c, err := New(config.Embedder{BaseURL: srv.URL, Model: "m", Dim: 768})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(seen) != 1 || seen[0] != 768 {
		t.Fatalf("dimensions sent = %v, want [768]", seen)
	}

	if c.Dim() != 768 {
		t.Fatalf("dim = %d want 768", c.Dim())
	}
}

// TestEmbedOmitsDimensionWhenUnset keeps the parameter off the wire by default:
// several providers reject a "dimensions" field outright, and the zero value
// must not opt them in.
func TestEmbedOmitsDimensionWhenUnset(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)

		_ = json.NewEncoder(w).Encode(embedResponse{Data: []struct {
			Embedding []float32 `json:"embedding"`
		}{{Embedding: []float32{1, 2}}}})
	}))
	defer srv.Close()

	c, _ := New(config.Embedder{BaseURL: srv.URL, Model: "m"})

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if _, ok := got["dimensions"]; ok {
		t.Fatalf("request carried a dimensions field: %v", got)
	}
}

// TestEmbedFallsBackWhenDimensionRejected covers the local-server case: an
// endpoint that 400s on the parameter must not fail the whole run, and must be
// asked only once.
func TestEmbedFallsBackWhenDimensionRejected(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
		withDims int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		attempts++
		if req.Dimensions > 0 {
			withDims++
		}
		mu.Unlock()

		if req.Dimensions > 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field: dimensions"}}`))

			return
		}

		var resp embedResponse
		for range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: []float32{1, 2, 3}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Concurrency 1 makes the batch order deterministic, so the count below
	// measures the latch rather than how many batches happened to be in
	// flight when the endpoint refused the parameter.
	c, _ := New(config.Embedder{BaseURL: srv.URL, Model: "m", Dim: 768, Batch: 1, Concurrency: 1})

	out, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(out) != 2 {
		t.Fatalf("got %d vectors want 2", len(out))
	}

	mu.Lock()
	defer mu.Unlock()

	if withDims != 1 {
		t.Fatalf("sent dimensions %d times, want 1 (the rejection must latch)", withDims)
	}

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (one rejected, one replayed, one for the second input)", attempts)
	}

	// The response is authoritative: the endpoint ignored the request, so the
	// store will see 3-wide vectors and the client must say so.
	if c.Dim() != 3 {
		t.Fatalf("dim = %d want 3 (the width actually returned)", c.Dim())
	}
}

// TestEmbedRejectsOversizedResponse turns a truncated body into a diagnosable
// error. A full batch of wide vectors is the realistic way to hit the read cap,
// and the JSON decode failure it used to produce named neither cause nor fix.
func TestEmbedRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[`))

		chunk := strings.Repeat("0.123456789,", 4096)
		for written := 0; written < maxRespBytes; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	c, _ := New(config.Embedder{BaseURL: srv.URL, Model: "m"})

	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected an error for an oversized response")
	}

	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want it to name the size problem", err)
	}
}

func TestPing(t *testing.T) {
	srv := embedServer(t, [][]float32{{1, 2, 3, 4}})
	defer srv.Close()

	c, _ := New(config.Embedder{BaseURL: srv.URL, Model: "m"})

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if c.Dim() != 4 {
		t.Fatalf("dim = %d want 4", c.Dim())
	}
}

func TestEmbedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	c, _ := New(config.Embedder{BaseURL: srv.URL, Model: "m"})

	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error on 500")
	}
}
