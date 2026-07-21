package repofs

import (
	"os"
	"path/filepath"
	"testing"
)

func setupRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	mustWrite(t, filepath.Join(dir, "listener", "processor.go"), "package listener\n")
	mustWrite(t, filepath.Join(dir, ".git", "config"), "[core]\n")
	mustWrite(t, filepath.Join(dir, "graphify-out", "graph.json"), "{}")

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

func TestListFilesShallowSkipsNoise(t *testing.T) {
	dir := setupRepo(t)

	entries, err := ListFiles(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if e.Path == ".git" || e.Path == "graphify-out" {
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
		if e.Path == ".git" || e.Path == filepath.Join("graphify-out", "graph.json") {
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
