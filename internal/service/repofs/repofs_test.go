package repofs

import (
	"fmt"
	"io/fs"
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

// deepRepo builds a tree with more entries than MaxListEntries — the cap that
// used to make the tail of a recursive listing unreachable — and returns every
// path the listing must eventually yield.
func deepRepo(t *testing.T) (string, map[string]bool) {
	t.Helper()

	dir := t.TempDir()
	want := map[string]bool{}

	for i := range 60 {
		top := fmt.Sprintf("pkg%02d", i)
		want[top] = true

		for j := range 2 {
			sub := fmt.Sprintf("%s/sub%d", top, j)
			want[sub] = true

			for k := range 20 {
				rel := fmt.Sprintf("%s/f%02d.go", sub, k)
				want[rel] = true
				mustWrite(t, filepath.Join(dir, filepath.FromSlash(rel)), "package p\n")
			}
		}
	}

	return dir, want
}

// collectPages walks every page of a cursor listing and returns the entries in
// the order they were served.
func collectPages(t *testing.T, dir string, recursive bool, perPage int) []Entry {
	t.Helper()

	var (
		all    []Entry
		cursor string
	)

	for pages := 1; ; pages++ {
		if pages > 1000 {
			t.Fatal("cursor paging did not terminate")
		}

		page, err := ListFilesCursor(dir, "", cursor, recursive, perPage)
		if err != nil {
			t.Fatal(err)
		}

		if page.PerPage != perPage {
			t.Fatalf("page %d per_page = %d, want %d", pages, page.PerPage, perPage)
		}

		all = append(all, page.Entries...)

		if page.NextCursor == "" {
			return all
		}

		if len(page.Entries) != perPage {
			t.Fatalf("page %d served %d of %d entries yet promised more", pages, len(page.Entries), perPage)
		}

		cursor = page.NextCursor
	}
}

func TestListFilesCursorPagesOneDirectory(t *testing.T) {
	t.Parallel()

	dir := setupRepo(t)

	page, err := ListFilesCursor(dir, "", "", false, 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Entries) != 1 || page.NextCursor == "" || page.PerPage != 1 {
		t.Fatalf("unexpected first page: %+v", page)
	}

	next, err := ListFilesCursor(dir, "", page.NextCursor, false, 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(next.Entries) != 1 || next.Entries[0].Path == page.Entries[0].Path {
		t.Fatalf("unexpected second page: %+v", next)
	}

	if next.NextCursor != "" {
		t.Fatalf("listing of two entries did not end: %+v", next)
	}
}

// The defect this pager replaced: the page-number listing built the whole
// sorted listing, truncated it at MaxListEntries and sliced a page out of that,
// so no request could reach an entry past the cap. Every entry of a tree larger
// than the cap must now be served exactly once.
func TestListFilesCursorReachesEveryEntryPastTheOldCap(t *testing.T) {
	t.Parallel()

	dir, want := deepRepo(t)
	if len(want) <= MaxListEntries {
		t.Fatalf("fixture has %d entries, needs more than the %d cap", len(want), MaxListEntries)
	}

	got := map[string]bool{}

	for _, e := range collectPages(t, dir, true, 100) {
		if got[e.Path] {
			t.Fatalf("entry %s served twice", e.Path)
		}

		got[e.Path] = true

		if !want[e.Path] {
			t.Fatalf("entry %s is not in the tree", e.Path)
		}
	}

	if len(got) != len(want) {
		for p := range want {
			if !got[p] {
				t.Fatalf("entry %s was never served (%d of %d reached)", p, len(got), len(want))
			}
		}
	}
}

// Listings order directories before files, then by path, and paging must not
// break that: a client rendering a tree from consecutive pages depends on it.
func TestListFilesCursorOrdersDirectoriesBeforeFilesAcrossPages(t *testing.T) {
	t.Parallel()

	dir, _ := deepRepo(t)

	entries := collectPages(t, dir, true, 100)
	if len(entries) < 2 {
		t.Fatalf("expected a populated listing, got %d entries", len(entries))
	}

	for i := 1; i < len(entries); i++ {
		if compareEntries(entries[i-1], entries[i]) >= 0 {
			t.Fatalf("entry %d (%+v) does not sort before entry %d (%+v)", i-1, entries[i-1], i, entries[i])
		}
	}

	if !entries[0].IsDir || entries[len(entries)-1].IsDir {
		t.Fatalf("listing must run directories first then files, got %+v .. %+v", entries[0], entries[len(entries)-1])
	}
}

// Resuming must skip whole subtrees whose every path is already behind the
// cursor, otherwise each page costs a full walk again. This test is not
// parallel: it swaps the package-level readDir seam to count directory reads.
func TestListFilesCursorSkipsSubtreesBehindTheCursor(t *testing.T) {
	dir, _ := deepRepo(t)

	// The cursor of the final page: everything before it is behind the cursor.
	last := "f:pkg59/sub1/f19.go"

	var reads int

	original := readDir
	readDir = func(fsys fs.FS, name string) ([]fs.DirEntry, error) {
		reads++

		return original(fsys, name)
	}

	t.Cleanup(func() { readDir = original })

	page, err := ListFilesCursor(dir, "", last, true, 100)
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Entries) != 0 || page.NextCursor != "" {
		t.Fatalf("page after the last entry should be empty, got %+v", page)
	}

	// The tree holds 181 directories. Resuming at the last path may only read
	// the root and the chain leading to it.
	if reads > 5 {
		t.Fatalf("resuming read %d directories, want the pruned path (<=5)", reads)
	}
}

// A cursor is opaque and must be handed back verbatim. Guessing at an
// unrecognised one would silently skip or repeat entries, so it is refused.
func TestListFilesCursorRejectsAForeignCursor(t *testing.T) {
	t.Parallel()

	dir := setupRepo(t)

	if _, err := ListFilesCursor(dir, "", "main.go", false, 10); err == nil {
		t.Fatal("a cursor without its phase prefix must be refused")
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
