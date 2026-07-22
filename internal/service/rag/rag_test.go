package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// fakeEmbedServer is an OpenAI-compatible /embeddings endpoint producing a
// deterministic 3-dim "topic" vector: counts of the words alpha/beta/gamma.
// Texts about the same topic get similar vectors.
func fakeEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		type datum struct {
			Embedding []float32 `json:"embedding"`
		}

		var resp struct {
			Data []datum `json:"data"`
		}

		for _, text := range req.Input {
			lower := strings.ToLower(text)
			vec := []float32{
				float32(strings.Count(lower, "alpha")) + 0.01,
				float32(strings.Count(lower, "beta")) + 0.01,
				float32(strings.Count(lower, "gamma")) + 0.01,
			}
			resp.Data = append(resp.Data, datum{Embedding: vec})
		}

		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func newTestService(t *testing.T, docsDirs map[string]string) *Service {
	t.Helper()

	srv := fakeEmbedServer(t)
	t.Cleanup(srv.Close)

	emb, err := embedder.New(config.Embedder{BaseURL: srv.URL, Model: "fake"})
	if err != nil {
		t.Fatalf("embedder.New: %v", err)
	}

	cfg := config.RAG{ChunkSize: 200, ChunkOverlap: 40, TopK: 20, TopDocs: 5}

	store, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("vectorstore.New: %v", err)
	}

	t.Cleanup(func() { _ = store.Close() })

	resolver := func(_ context.Context, repo string) (string, error) {
		return docsDirs[repo], nil
	}

	return New(cfg, emb, store, resolver)
}

func writeDocs(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestIndexAndRetrieveWholeDoc(t *testing.T) {
	ctx := context.Background()

	docsDir := writeDocs(t, map[string]string{
		"alpha.md":     "# Alpha Service\n\nalpha alpha alpha handles alpha things.\n",
		"beta.md":      "# Beta Worker\n\nbeta beta beta processes beta jobs.\n",
		"sub/gamma.md": "# Gamma Client\n\ngamma gamma gamma calls gamma APIs.\n",
	})

	s := newTestService(t, map[string]string{"o/r": docsDir})

	if err := s.Index(ctx, "o/r", docsDir); err != nil {
		t.Fatalf("Index: %v", err)
	}

	docs, err := s.Retrieve(ctx, "o/r", "tell me about beta", 1)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("got %d docs want 1", len(docs))
	}

	d := docs[0]
	if d.Path != "beta.md" || d.Repo != "o/r" {
		t.Fatalf("wrong doc: %+v", d)
	}

	if d.Title != "Beta Worker" {
		t.Fatalf("title = %q", d.Title)
	}

	// Whole file, not just the matching chunk.
	if !strings.Contains(d.Content, "# Beta Worker") || !strings.Contains(d.Content, "processes beta jobs") {
		t.Fatalf("content is not the whole doc: %q", d.Content)
	}

	// Nested docs are reachable too.
	docs, err = s.Retrieve(ctx, "o/r", "gamma question", 1)
	if err != nil {
		t.Fatalf("Retrieve gamma: %v", err)
	}

	if len(docs) != 1 || docs[0].Path != "sub/gamma.md" {
		t.Fatalf("gamma doc = %+v", docs)
	}
}

func TestIndexRemovesStaleDocs(t *testing.T) {
	ctx := context.Background()

	docsDir := writeDocs(t, map[string]string{
		"alpha.md": "# Alpha\n\nalpha alpha alpha\n",
		"beta.md":  "# Beta\n\nbeta beta beta\n",
	})

	s := newTestService(t, map[string]string{"o/r": docsDir})

	if err := s.Index(ctx, "o/r", docsDir); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// beta.md disappears; re-index must drop its vectors.
	if err := os.Remove(filepath.Join(docsDir, "beta.md")); err != nil {
		t.Fatal(err)
	}

	if err := s.Index(ctx, "o/r", docsDir); err != nil {
		t.Fatalf("re-Index: %v", err)
	}

	docs, err := s.Retrieve(ctx, "o/r", "beta beta beta", 5)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	for _, d := range docs {
		if d.Path == "beta.md" {
			t.Fatalf("stale doc still retrieved: %+v", d)
		}
	}
}

func TestRetrieveAcrossRepos(t *testing.T) {
	ctx := context.Background()

	dirA := writeDocs(t, map[string]string{"a.md": "# A\n\nalpha alpha alpha\n"})
	dirB := writeDocs(t, map[string]string{"b.md": "# B\n\nbeta beta beta\n"})

	s := newTestService(t, map[string]string{"o/a": dirA, "o/b": dirB})

	if err := s.Index(ctx, "o/a", dirA); err != nil {
		t.Fatalf("Index a: %v", err)
	}

	if err := s.Index(ctx, "o/b", dirB); err != nil {
		t.Fatalf("Index b: %v", err)
	}

	// repo == "" searches all repos.
	docs, err := s.Retrieve(ctx, "", "beta", 1)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if len(docs) != 1 || docs[0].Repo != "o/b" {
		t.Fatalf("cross-repo docs = %+v", docs)
	}
}

func TestRetrieveEmptyQuestion(t *testing.T) {
	s := newTestService(t, nil)

	if _, err := s.Retrieve(context.Background(), "", "  ", 5); err == nil {
		t.Fatal("expected error for empty question")
	}
}
