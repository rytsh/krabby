package embedder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
