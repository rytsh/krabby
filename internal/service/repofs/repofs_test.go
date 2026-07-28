package repofs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func setupRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	mustWrite(t, filepath.Join(dir, "listener", "processor.go"), "package listener\n")
	mustWrite(t, filepath.Join(dir, ".git", "config"), "[core]\n")
	mustWrite(t, filepath.Join(dir, "graphify-out", "graph.json"), "{}")
	mustWrite(t, filepath.Join(dir, "vendor", "dep", "lib.go"), "package dep\n")

	// A secret outside the repo that traversal attempts must never reach.
	mustWrite(t, filepath.Join(filepath.Dir(dir), "secret.txt"), "TOP SECRET")

	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadFile(t *testing.T) {
	dir := setupRepo(t)

	fc, err := ReadFile(dir, "listener/processor.go", 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if fc.Content != "package listener\n" {
		t.Fatalf("unexpected content: %q", fc.Content)
	}

	if fc.Truncated {
		t.Fatal("small file should not be truncated")
	}
}

func TestReadFileTraversalRejected(t *testing.T) {
	dir := setupRepo(t)

	for _, bad := range []string{
		"../secret.txt",
		"../../secret.txt",
		"listener/../../secret.txt",
		"/etc/passwd",
	} {
		if _, err := ReadFile(dir, bad, 0, 0); err == nil {
			t.Fatalf("expected traversal %q to be rejected", bad)
		}
	}
}

func TestReadFilePagination(t *testing.T) {
	dir := setupRepo(t)
	mustWrite(t, filepath.Join(dir, "big.txt"), "0123456789")

	fc, err := ReadFile(dir, "big.txt", 0, 4)
	if err != nil {
		t.Fatal(err)
	}

	if fc.Content != "0123" || !fc.Truncated || fc.TotalSize != 10 {
		t.Fatalf("unexpected page1: %+v", fc)
	}

	fc2, err := ReadFile(dir, "big.txt", 4, 100)
	if err != nil {
		t.Fatal(err)
	}

	if fc2.Content != "456789" || fc2.Truncated {
		t.Fatalf("unexpected page2: %+v", fc2)
	}
}

func TestListFilesPage(t *testing.T) {
	dir := setupRepo(t)
	page, err := ListFilesPage(dir, "", false, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || !page.HasMore || page.Page != 1 || page.PerPage != 1 {
		t.Fatalf("unexpected first page: %+v", page)
	}

	next, err := ListFilesPage(dir, "", false, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Entries) != 1 || next.Entries[0].Path == page.Entries[0].Path {
		t.Fatalf("unexpected second page: %+v", next)
	}
}

func TestListFilesShallowSkipsNoise(t *testing.T) {
	dir := setupRepo(t)

	entries, err := ListFiles(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if e.Path == ".git" || e.Path == "graphify-out" || e.Path == "vendor" {
			t.Fatalf("listing must skip %s", e.Path)
		}
	}

	var sawMain, sawListener bool

	for _, e := range entries {
		if e.Path == "main.go" && !e.IsDir {
			sawMain = true
		}

		if e.Path == "listener" && e.IsDir {
			sawListener = true
		}
	}

	if !sawMain || !sawListener {
		t.Fatalf("expected main.go and listener/ in listing, got %+v", entries)
	}
}

func TestListFilesRecursiveSkipsNoise(t *testing.T) {
	dir := setupRepo(t)

	entries, err := ListFiles(dir, "", true)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if e.Path == ".git" ||
			e.Path == filepath.Join("graphify-out", "graph.json") ||
			e.Path == filepath.Join("vendor", "dep", "lib.go") {
			t.Fatalf("recursive listing must skip %s", e.Path)
		}
	}

	var sawNested bool

	for _, e := range entries {
		if e.Path == "listener/processor.go" {
			sawNested = true
		}
	}

	if !sawNested {
		t.Fatalf("expected nested file in recursive listing, got %+v", entries)
	}
}

func TestDeployConfigFile(t *testing.T) {
	tests := []struct {
		rel  string
		want bool
	}{
		// Compose, with and without an environment in the middle.
		{"docker-compose.yml", true},
		{"docker-compose.yaml", true},
		{"compose.yaml", true},
		{"docker-compose.prod.yml", true},
		{"docker-compose-sandbox.yml", true},
		{"compose.override.yml", true},
		{"deploy/stage/docker-compose.yml", true},
		{"Docker-Compose.PROD.YML", true},
		{"docker-stack.prod.yml", true},

		// Helm.
		{"chart.yaml", true},
		{"values.yaml", true},
		{"values-prod.yaml", true},
		{"charts/api/values.stage.yaml", true},

		// CI.
		{".gitlab-ci.yml", true},
		{".github/workflows/release.yml", true},
		{"sub/module/.github/workflows/ci.yaml", true},

		// Not deploy config: arbitrary YAML stays out, which is the whole
		// point of matching families rather than the extension.
		{"deployment.yaml", false},
		{"k8s/service.yaml", false},
		{"openapi.yaml", false},
		{"config/app.yml", false},
		{"docker-compose.md", false},
		{"values.json", false},
		{"docs/.github/notes.yml", false},
	}

	for _, tt := range tests {
		if got := DeployConfigFile(tt.rel); got != tt.want {
			t.Errorf("DeployConfigFile(%q) = %v, want %v", tt.rel, got, tt.want)
		}
	}
}

// WalkFiles exists because ListFiles caps at MaxListEntries, which silently
// truncates any caller that must see the whole repository.
func TestWalkFilesIsUncapped(t *testing.T) {
	dir := t.TempDir()

	const files = MaxListEntries + 25
	for i := range files {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("pkg%04d", i), "f.go"), "package p\n")
	}

	var seen int
	if err := WalkFiles(dir, nil, func(string, int64) error {
		seen++

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if seen != files {
		t.Fatalf("walked %d files, want %d", seen, files)
	}

	if listed, err := ListFiles(dir, "", true); err != nil {
		t.Fatal(err)
	} else if len(listed) != MaxListEntries {
		t.Fatalf("ListFiles should still cap at %d, got %d", MaxListEntries, len(listed))
	}
}

// Only .git is pruned unconditionally; everything else is the caller's call, so
// an explicit "index vendor/" stays expressible.
func TestWalkFilesPruningIsCallerControlled(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(dir, "vendor", "dep", "lib.go"), "package dep\n")
	mustWrite(t, filepath.Join(dir, "skipme", "x.go"), "package x\n")
	mustWrite(t, filepath.Join(dir, ".git", "config"), "[core]\n")

	collect := func(skip func(rel, name string) bool) []string {
		var out []string
		if err := WalkFiles(dir, skip, func(rel string, _ int64) error {
			out = append(out, rel)

			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)

		return out
	}

	// No filter: vendor/ is walked (ListFiles hides it; WalkFiles must not).
	got := collect(nil)
	if len(got) != 3 || got[0] != "main.go" {
		t.Fatalf("walked %v, want main.go plus vendor and skipme, and never .git", got)
	}
	for _, rel := range got {
		if strings.HasPrefix(rel, ".git/") {
			t.Fatalf(".git must always be pruned, got %v", got)
		}
	}

	// With a filter the caller prunes what it wants.
	got = collect(func(_, name string) bool { return name == "skipme" })
	for _, rel := range got {
		if strings.HasPrefix(rel, "skipme/") {
			t.Fatalf("skipDir was ignored, got %v", got)
		}
	}
}
