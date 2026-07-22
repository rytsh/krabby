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

func TestGenerateWritesDocAndManifest(t *testing.T) {
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

	// One user-facing doc (documentation.md); two internal summaries.
	if len(man.Docs) != 1 || man.Docs[0].Path != DocName {
		t.Fatalf("docs = %+v, want single %s", man.Docs, DocName)
	}
	if len(man.Summaries) != 2 {
		t.Fatalf("summaries = %d, want 2: %+v", len(man.Summaries), man.Summaries)
	}

	if _, err := os.Stat(filepath.Join(docsDir, DocName)); err != nil {
		t.Errorf("expected %s: %v", DocName, err)
	}

	for _, rel := range []string{"main.go.sum", "pkg/util.go.sum"} {
		p := filepath.Join(docsDir, summariesDir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected summary %s: %v", rel, err)
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

	// Two summaries + one synthesis.
	if calls != 3 {
		t.Errorf("llm calls = %d, want 3", calls)
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
	if calls != 3 {
		t.Fatalf("first run calls = %d, want 3 (2 summaries + synthesis)", calls)
	}

	// Change only b.go; a.go summary is reused, b.go + synthesis regenerate.
	writeSrc(t, clone, "b.go", "package b\nfunc B() {}\n")
	if _, err := gen.Generate(context.Background(), "r", clone, docsDir); err != nil {
		t.Fatalf("second gen: %v", err)
	}
	if calls != 5 {
		t.Errorf("after change, calls = %d, want 5 (b.go + synthesis)", calls)
	}

	// Nothing changed: no LLM calls at all (summaries cached, doc reused).
	if _, err := gen.Generate(context.Background(), "r", clone, docsDir); err != nil {
		t.Fatalf("third gen: %v", err)
	}
	if calls != 5 {
		t.Errorf("unchanged run made llm calls: %d, want 5", calls)
	}
}

func TestMigratesOldPerFileLayout(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "a.go", "package a\n")

	docsDir := filepath.Join(clone, "krabby-docs")

	// Simulate the pre-synthesis layout: per-file doc listed in Docs with its
	// markdown at "<rel>.md".
	srcHash := func() string {
		b, _ := os.ReadFile(filepath.Join(clone, "a.go"))
		return hashString(string(b))
	}()

	writeSrc(t, docsDir, "a.go.md", "## a.go\nold per-file doc\n")
	writeSrc(t, docsDir, "overview.md", "# old overview\n")
	oldMan := &Manifest{
		Repo:  "r",
		Model: "test",
		Docs: []DocMeta{
			{Path: "a.go.md", Title: "a.go", SourcePath: "a.go", SourceHash: srcHash},
			{Path: "overview.md", Title: "Overview"},
		},
	}
	if err := writeManifest(docsDir, oldMan); err != nil {
		t.Fatal(err)
	}

	var calls int32
	gen := New(config.Docs{}, fakeLLM(t, &calls), nil)

	man, err := gen.Generate(context.Background(), "r", clone, docsDir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Cached per-file content migrated: only the synthesis call runs.
	if calls != 1 {
		t.Errorf("llm calls = %d, want 1 (summary reused from old layout)", calls)
	}

	if len(man.Docs) != 1 || man.Docs[0].Path != DocName {
		t.Fatalf("docs = %+v, want single %s", man.Docs, DocName)
	}

	// Old user-facing markdown cleaned up.
	if _, err := os.Stat(filepath.Join(docsDir, "a.go.md")); !os.IsNotExist(err) {
		t.Errorf("old per-file doc not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(docsDir, "overview.md")); !os.IsNotExist(err) {
		t.Errorf("old overview not cleaned up")
	}

	// Migrated summary exists.
	if _, err := os.Stat(filepath.Join(docsDir, summariesDir, "a.go.sum")); err != nil {
		t.Errorf("migrated summary missing: %v", err)
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

	if len(man.Summaries) != 1 || man.Summaries[0].SourcePath != "keep.go" {
		t.Fatalf("expected only keep.go summarized, got %+v", man.Summaries)
	}
}

func TestCustomPromptUsedForSynthesis(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "x.go", "package x\n")

	var systems []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				systems = append(systems, m.Content)
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

	// The custom prompt applies to the synthesis call (the last one); the
	// summary phase always uses the fixed internal prompt.
	if len(systems) != 2 {
		t.Fatalf("system prompts = %d, want 2", len(systems))
	}
	if systems[0] != summaryPrompt {
		t.Errorf("summary phase should use the internal prompt, got %q", systems[0])
	}
	if !strings.Contains(systems[1], "XYZZY") {
		t.Errorf("custom prompt not used for synthesis; got %q", systems[1])
	}
}

func TestDefaultPromptFallback(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "y.go", "package y\n")

	var systems []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				systems = append(systems, m.Content)
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

	if len(systems) == 0 || systems[len(systems)-1] != DefaultPrompt {
		t.Errorf("expected default synthesis prompt as last system message")
	}
}

func TestDefaultPromptForbidsNestedMermaidDoubleQuotes(t *testing.T) {
	if !strings.Contains(DefaultPrompt, "Never put another literal or escaped double quote inside an already quoted") {
		t.Fatal("default prompt must prevent nested-quote Mermaid parse failures")
	}
}
