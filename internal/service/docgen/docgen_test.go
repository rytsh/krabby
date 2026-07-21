package docgen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/llm"
)

// fakeLLM returns a server that echoes a deterministic markdown body and counts
// calls, so tests can assert incremental regeneration.
func fakeLLM(t *testing.T, calls *int32) *llm.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(calls, 1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"# Doc\n\ngenerated"}}]}`))
	}))
	t.Cleanup(srv.Close)

	c, err := llm.New(config.LLM{BaseURL: srv.URL, Model: "test"})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	return c
}

func writeSrc(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateWritesDocsAndManifest(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "main.go", "package main\nfunc main() {}\n")
	writeSrc(t, clone, "pkg/util.go", "package pkg\nfunc Util() {}\n")
	writeSrc(t, clone, "README.md", "# not code\n") // excluded by default ext allowlist

	var calls int32
	gen := New(config.Docs{}, fakeLLM(t, &calls), nil)

	docsDir := filepath.Join(clone, "krabby-docs")
	man, err := gen.Generate(context.Background(), "owner/repo", clone, docsDir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Two .go files documented (README.md excluded); no overview (nil engine).
	if len(man.Docs) != 2 {
		t.Fatalf("docs = %d, want 2: %+v", len(man.Docs), man.Docs)
	}

	for _, rel := range []string{"main.go.md", "pkg/util.go.md"} {
		p := filepath.Join(docsDir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected doc %s: %v", rel, err)
		}
	}

	// Manifest on disk parses.
	b, err := os.ReadFile(filepath.Join(docsDir, ManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var parsed Manifest
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("manifest json: %v", err)
	}
	if parsed.Repo != "owner/repo" || parsed.Model != "test" {
		t.Errorf("manifest meta wrong: %+v", parsed)
	}

	if calls != 2 {
		t.Errorf("llm calls = %d, want 2", calls)
	}
}

func TestGenerateIncremental(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "a.go", "package a\n")
	writeSrc(t, clone, "b.go", "package b\n")

	var calls int32
	gen := New(config.Docs{}, fakeLLM(t, &calls), nil)
	docsDir := filepath.Join(clone, "krabby-docs")

	if _, err := gen.Generate(context.Background(), "r", clone, docsDir); err != nil {
		t.Fatalf("first gen: %v", err)
	}
	if calls != 2 {
		t.Fatalf("first run calls = %d, want 2", calls)
	}

	// Change only b.go; a.go should be reused, b.go regenerated.
	writeSrc(t, clone, "b.go", "package b\nfunc B() {}\n")
	if _, err := gen.Generate(context.Background(), "r", clone, docsDir); err != nil {
		t.Fatalf("second gen: %v", err)
	}
	if calls != 3 {
		t.Errorf("after change, calls = %d, want 3 (only b.go regenerated)", calls)
	}
}

func TestIncludeExcludeGlobs(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "keep.go", "package a\n")
	writeSrc(t, clone, "skip.go", "package a\n")
	writeSrc(t, clone, "vendor/dep.go", "package v\n")

	var calls int32
	gen := New(config.Docs{
		Include: []string{"*.go"},
		Exclude: []string{"skip.go", "vendor/"},
	}, fakeLLM(t, &calls), nil)

	docsDir := filepath.Join(clone, "krabby-docs")
	man, err := gen.Generate(context.Background(), "r", clone, docsDir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(man.Docs) != 1 || man.Docs[0].SourcePath != "keep.go" {
		t.Fatalf("expected only keep.go, got %+v", man.Docs)
	}
}

func TestCustomPromptUsed(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "x.go", "package x\n")

	var gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				gotSystem = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"# d"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.New(config.LLM{BaseURL: srv.URL, Model: "m"})
	gen := New(config.Docs{Prompt: "CUSTOM PROMPT XYZZY"}, c, nil)

	if _, err := gen.Generate(context.Background(), "r", clone, filepath.Join(clone, "krabby-docs")); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.Contains(gotSystem, "XYZZY") {
		t.Errorf("custom prompt not used; system = %q", gotSystem)
	}
}

func TestDefaultPromptFallback(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "y.go", "package y\n")

	var gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				gotSystem = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"# d"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.New(config.LLM{BaseURL: srv.URL, Model: "m"})
	gen := New(config.Docs{Prompt: "   "}, c, nil) // blank -> default

	if _, err := gen.Generate(context.Background(), "r", clone, filepath.Join(clone, "krabby-docs")); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if gotSystem != DefaultPrompt {
		t.Errorf("expected default prompt, got %q", gotSystem)
	}
}
