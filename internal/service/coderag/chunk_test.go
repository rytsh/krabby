package coderag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func TestParseLine(t *testing.T) {
	t.Parallel()

	tests := map[string]int{
		"L42":       42,
		"L42-L60":   42,
		"line 7":    7,
		"Line 8:12": 8,
		"12:5":      12,
		"unknown":   0,
		"L0":        0,
	}

	for input, want := range tests {
		if got := parseLine(input); got != want {
			t.Errorf("parseLine(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestGlobMatchDoublestar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/service/main.go", true},
		{"**/generated/**", "pkg/generated/client/api.go", true},
		{"vendor/**", "vendor/lib/code.go", true},
		{"src/*.go", "src/nested/main.go", false},
		{"**/*.go", "README.md", false},
	}

	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.name); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestChunkFileUsesSymbolBoundaries(t *testing.T) {
	t.Parallel()

	content := strings.Join([]string{
		"package service",
		"",
		"import \"context\"",
		"",
		"func Authenticate(ctx context.Context) error {",
		"    return nil",
		"}",
		"",
		"func Authorize(ctx context.Context) bool {",
		"    return true",
		"}",
	}, "\n")

	chunks := chunkFile(content, []symbol{
		{Name: "Authenticate", Line: 5},
		{Name: "Authorize", Line: 9},
	}, 90, 20)

	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: %#v", len(chunks), chunks)
	}

	if chunks[0].StartLine != 1 || chunks[0].EndLine != 4 {
		t.Errorf("preamble range = %d-%d, want 1-4", chunks[0].StartLine, chunks[0].EndLine)
	}

	if chunks[1].Symbol != "Authenticate" || chunks[1].StartLine != 5 || chunks[1].EndLine != 8 {
		t.Errorf("authenticate chunk = %#v", chunks[1])
	}

	if chunks[2].Symbol != "Authorize" || chunks[2].StartLine != 9 || chunks[2].EndLine != 11 {
		t.Errorf("authorize chunk = %#v", chunks[2])
	}
}

func TestCodeRAGIndexAndRetrieve(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		data := make([]map[string]any, len(req.Input))
		for i, input := range req.Input {
			vec := []float32{0, 1}
			if strings.Contains(strings.ToLower(input), "authenticate") {
				vec = []float32{1, 0}
			}
			data[i] = map[string]any{"embedding": vec}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "auth.go"), []byte("package auth\n\nfunc Authenticate(token string) bool { return token != \"\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "math.go"), []byte("package math\n\nfunc Add(a, b int) int { return a + b }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.CodeRAG{ChunkSize: 3000, ChunkOverlap: 1000, TopK: 1}
	emb, err := embedder.New(config.Embedder{BaseURL: server.URL, Model: "test", Batch: 64})
	if err != nil {
		t.Fatal(err)
	}

	store, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	svc := New(cfg, emb, store, nil, nil)
	if err := svc.Index(context.Background(), "acme/app", root); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Retrieve(context.Background(), "acme/app", "where is Authenticate handled?", 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %s", len(got), fmt.Sprint(got))
	}

	if got[0].Path != "auth.go" || got[0].StartLine != 1 || !strings.Contains(got[0].Snippet, "Authenticate") {
		t.Errorf("unexpected result: %#v", got[0])
	}
}

func TestIndexChangedIncremental(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		data := make([]map[string]any, len(req.Input))
		for i, input := range req.Input {
			vec := []float32{0, 1}
			if strings.Contains(strings.ToLower(input), "authorize") {
				vec = []float32{1, 0}
			}
			data[i] = map[string]any{"embedding": vec}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("auth.go", "package auth\n\nfunc Authenticate(token string) bool { return token != \"\" }\n")
	write("math.go", "package math\n\nfunc Add(a, b int) int { return a + b }\n")

	cfg := config.CodeRAG{ChunkSize: 3000, ChunkOverlap: 1000, TopK: 1}
	emb, err := embedder.New(config.Embedder{BaseURL: server.URL, Model: "test", Batch: 64})
	if err != nil {
		t.Fatal(err)
	}

	store, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	text, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	svc := New(cfg, emb, store, nil, text)
	ctx := context.Background()
	if err := svc.Index(ctx, "acme/app", root); err != nil {
		t.Fatal(err)
	}

	// Change auth.go, delete math.go, add greet.go; only pass those as changed.
	write("auth.go", "package auth\n\nfunc Authorize(role string) bool { return role == \"admin\" }\n")
	if err := os.Remove(filepath.Join(root, "math.go")); err != nil {
		t.Fatal(err)
	}
	write("greet.go", "package greet\n\nfunc Hello() string { return \"hi\" }\n")

	if err := svc.IndexChanged(ctx, "acme/app", root, []string{"auth.go", "math.go", "greet.go"}); err != nil {
		t.Fatal(err)
	}

	// Semantic: the updated auth.go content is retrievable.
	got, err := svc.Retrieve(ctx, "acme/app", "authorize admin role", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "auth.go" || !strings.Contains(got[0].Snippet, "Authorize") {
		t.Fatalf("unexpected semantic result: %#v", got)
	}

	// FTS: deleted file gone, old content gone, new file searchable.
	if page, err := text.Search(ctx, "", "Add", 1, 20); err != nil || page.Total != 0 {
		t.Fatalf("deleted file still searchable: %#v, %v", page, err)
	}
	if page, err := text.Search(ctx, "", "Authenticate", 1, 20); err != nil || page.Total != 0 {
		t.Fatalf("stale content still searchable: %#v, %v", page, err)
	}
	if page, err := text.Search(ctx, "", "Hello", 1, 20); err != nil || page.Total != 1 || page.Results[0].Path != "greet.go" {
		t.Fatalf("new file not searchable: %#v, %v", page, err)
	}
}
