package docgen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/graphquery"
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

// modelRecordingLLM records every model name it was asked to complete with.
func modelRecordingLLM(t *testing.T, model string, seen *[]string, mu *sync.Mutex) *llm.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		*seen = append(*seen, req.Model)
		mu.Unlock()
		// Return two per-file sections so summary splitting works.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"## a.go\\nsummary a\\n\\n## b.go\\nsummary b"}}]}`))
	}))
	t.Cleanup(srv.Close)

	c, err := llm.New(config.LLM{BaseURL: srv.URL, Model: model})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	return c
}

func TestSummaryModelUsedForSummariesOnly(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "a.go", "package a\nfunc A() {}\n")
	writeSrc(t, clone, "b.go", "package b\nfunc B() {}\n")

	var (
		mu          sync.Mutex
		synthSeen   []string
		summarySeen []string
	)
	synth := modelRecordingLLM(t, "pro-model", &synthSeen, &mu)
	summary := modelRecordingLLM(t, "flash-model", &summarySeen, &mu)

	gen := New(config.Docs{}, synth, summary, nil)

	docsDir := filepath.Join(clone, "krabby-docs")
	if _, err := gen.Generate(context.Background(), "r", clone, docsDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Every summary call must use the flash model; there must be at least one.
	if len(summarySeen) == 0 {
		t.Fatal("summary model was never called")
	}
	for _, m := range summarySeen {
		if m != "flash-model" {
			t.Errorf("summary call used model %q, want flash-model", m)
		}
	}

	// Synthesis must use the pro model exactly once.
	if len(synthSeen) != 1 || synthSeen[0] != "pro-model" {
		t.Errorf("synthesis calls = %v, want one pro-model call", synthSeen)
	}
}

func TestNilSummaryFallsBackToChat(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "a.go", "package a\n")

	var mu sync.Mutex
	var seen []string
	chat := modelRecordingLLM(t, "only-model", &seen, &mu)

	gen := New(config.Docs{}, chat, nil, nil) // nil summary -> use chat for both

	if _, err := gen.Generate(context.Background(), "r", clone, filepath.Join(clone, "krabby-docs")); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("expected >=2 calls (summary + synthesis), got %d", len(seen))
	}
	for _, m := range seen {
		if m != "only-model" {
			t.Errorf("call used model %q, want only-model", m)
		}
	}
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
	gen := New(config.Docs{}, fakeLLM(t, &calls), nil, nil)

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

// writeGraph writes a minimal graphify graph.json into the clone's
// graphify-out dir, assigning the given files to communities via their symbol
// nodes. community maps a source file to its community id.
func writeGraph(t *testing.T, clone string, community map[string]int) {
	t.Helper()

	type node struct {
		ID         string `json:"id"`
		Label      string `json:"label"`
		SourceFile string `json:"source_file"`
		Community  int    `json:"community"`
	}

	var nodes []node
	i := 0
	for file, cid := range community {
		nodes = append(nodes, node{
			ID:         file + "#sym",
			Label:      "Sym" + file,
			SourceFile: file,
			Community:  cid,
		})
		i++
	}

	graph := map[string]any{"nodes": nodes, "links": []any{}}
	b, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(clone, "graphify-out", "graph.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateCommunityGroupingReducesCalls(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "a.go", "package a\nfunc A() {}\n")
	writeSrc(t, clone, "b.go", "package b\nfunc B() {}\n")

	// Both files live in the same community, so a single grouped summary call
	// should cover both, then one synthesis call: 2 LLM calls total (vs 3
	// without grouping).
	writeGraph(t, clone, map[string]int{"a.go": 0, "b.go": 0})

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Return per-file sections so splitSummarySections finds both files.
		body := "## a.go\\nsummary a\\n\\n## b.go\\nsummary b"
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + body + `"}}]}`))
	}))
	t.Cleanup(srv.Close)

	c, err := llm.New(config.LLM{BaseURL: srv.URL, Model: "test"})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	gen := New(config.Docs{}, c, nil, graphquery.NewEngine())

	docsDir := filepath.Join(clone, "krabby-docs")
	man, err := gen.Generate(context.Background(), "owner/repo", clone, docsDir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("llm calls = %d, want 2 (1 grouped summary + 1 synthesis)", got)
	}

	if len(man.Summaries) != 2 {
		t.Fatalf("summaries = %d, want 2: %+v", len(man.Summaries), man.Summaries)
	}

	// Both per-file summaries were split out and cached.
	for _, rel := range []string{"a.go.sum", "b.go.sum"} {
		p := filepath.Join(docsDir, summariesDir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected summary %s: %v", rel, err)
		}
	}
}

func TestBuildGroupsCapsGroupCount(t *testing.T) {
	// Simulate a highly fragmented graph: many tiny communities. buildGroups
	// must pack them so the number of grouped summary calls stays bounded.
	clone := t.TempDir()

	const numCommunities = 200

	files := make([]string, 0, numCommunities)
	community := map[string]int{}
	for i := range numCommunities {
		f := fmt.Sprintf("pkg%03d/file.go", i)
		writeSrc(t, clone, f, "package p\n")
		files = append(files, f)
		community[f] = i
	}
	sort.Strings(files)
	writeGraph(t, clone, community)

	gen := New(config.Docs{MaxGroups: 40}, nil, nil, graphquery.NewEngine()).(*llmGenerator)

	graph := gen.loadGraph(clone)
	if graph == nil {
		t.Fatal("graph did not load")
	}

	groups := gen.buildGroups(files, graph)

	if len(groups) > 40 {
		t.Fatalf("group count = %d, want <= 40 (MaxGroups)", len(groups))
	}

	// Every file must appear exactly once across all groups.
	seen := map[string]int{}
	for _, grp := range groups {
		if len(grp.files) > maxFilesPerGroup {
			t.Errorf("group has %d files, want <= %d", len(grp.files), maxFilesPerGroup)
		}
		for _, f := range grp.files {
			seen[f]++
		}
	}
	if len(seen) != numCommunities {
		t.Fatalf("covered %d files, want %d", len(seen), numCommunities)
	}
	for f, n := range seen {
		if n != 1 {
			t.Errorf("file %s appeared %d times, want 1", f, n)
		}
	}
}

func TestGenerateIncremental(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "a.go", "package a\n")
	writeSrc(t, clone, "b.go", "package b\n")

	var calls int32
	gen := New(config.Docs{}, fakeLLM(t, &calls), nil, nil)
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
	gen := New(config.Docs{}, fakeLLM(t, &calls), nil, nil)

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
	}, fakeLLM(t, &calls), nil, nil)

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
	gen := New(config.Docs{Prompt: "CUSTOM PROMPT XYZZY"}, c, nil, nil)

	if _, err := gen.Generate(context.Background(), "r", clone, filepath.Join(clone, "krabby-docs")); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// The custom prompt applies to the synthesis call (the last one); the
	// summary phase always uses the fixed internal prompt.
	if len(systems) != 2 {
		t.Fatalf("system prompts = %d, want 2", len(systems))
	}
	if systems[0] != groupSummaryPrompt {
		t.Errorf("summary phase should use the internal group prompt, got %q", systems[0])
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
	gen := New(config.Docs{Prompt: "   "}, c, nil, nil) // blank -> default

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

func TestDefaultPromptForbidsParensInMermaidLabels(t *testing.T) {
	// Guards the reported failure: F[File System (/share/*)] breaks the parser;
	// F[File System /share/*] is correct.
	for _, want := range []string{
		"F[File System /share/*]",
		"do NOT put\n  parentheses",
	} {
		if !strings.Contains(DefaultPrompt, want) {
			t.Fatalf("default prompt must warn against parentheses in Mermaid node labels; missing %q", want)
		}
	}
}

func TestDefaultPromptCoversBackendAndFrontendSections(t *testing.T) {
	for _, want := range []string{
		"BACKEND sections",
		"FRONTEND sections",
		"Database schema",
		"Component hierarchy",
		"CRITICAL RULES:",
	} {
		if !strings.Contains(DefaultPrompt, want) {
			t.Errorf("default prompt missing expected section/marker %q", want)
		}
	}
}

func TestSelectFilesSkipsTestAndFixtureNoise(t *testing.T) {
	clone := t.TempDir()
	// Documented sources.
	writeSrc(t, clone, "main.go", "package main\n")
	writeSrc(t, clone, "pkg/svc/svc.go", "package svc\n")
	writeSrc(t, clone, "go.mod", "module x\n")
	// Noise that must be skipped by default.
	writeSrc(t, clone, "pkg/svc/svc_test.go", "package svc\n")
	writeSrc(t, clone, "testdata/mock/loader.go", "package mock\n")
	writeSrc(t, clone, "internal/mocks/db.go", "package mocks\n")
	writeSrc(t, clone, "web/app.test.ts", "export {}\n")
	writeSrc(t, clone, "vendor/dep/dep.go", "package dep\n")

	gen := New(config.Docs{}, nil, nil, nil).(*llmGenerator)

	files, err := gen.selectFiles(clone)
	if err != nil {
		t.Fatalf("selectFiles: %v", err)
	}

	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}

	for _, want := range []string{"main.go", "pkg/svc/svc.go", "go.mod"} {
		if !got[want] {
			t.Errorf("expected %q to be documented, got %v", want, files)
		}
	}
	for _, skip := range []string{
		"pkg/svc/svc_test.go", "testdata/mock/loader.go",
		"internal/mocks/db.go", "web/app.test.ts", "vendor/dep/dep.go",
	} {
		if got[skip] {
			t.Errorf("expected %q to be skipped as noise, got %v", skip, files)
		}
	}
}

func TestSelectFilesRespectsExplicitInclude(t *testing.T) {
	// With an explicit Include set, the user is in control: test files match
	// the glob and are documented (no implicit noise filtering).
	clone := t.TempDir()
	writeSrc(t, clone, "svc.go", "package svc\n")
	writeSrc(t, clone, "svc_test.go", "package svc\n")

	gen := New(config.Docs{Include: []string{"*.go"}}, nil, nil, nil).(*llmGenerator)
	files, err := gen.selectFiles(clone)
	if err != nil {
		t.Fatalf("selectFiles: %v", err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got["svc_test.go"] {
		t.Errorf("explicit Include should document svc_test.go; got %v", files)
	}
}
