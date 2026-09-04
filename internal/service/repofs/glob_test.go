package repofs

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// globRepo lays out the shapes the anchoring rule has to distinguish: the same
// relative path (_ui/src/app.ts) at the root and nested one level down, plus
// the noise directories every walk must prune.
func globRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for _, rel := range []string{
		"Makefile",
		"main.go",
		"db/migrations/001_init.sql",
		"db/migrations/002_users.sql",
		"internal/service/repofs/repofs.go",
		"internal/service/Makefile",
		"_ui/src/app.ts",
		"_ui/src/lib/store.ts",
		"_ui/build/app.ts",
		"nested/_ui/src/app.ts",
		".git/config",
		"graphify-out/graph.json",
		"vendor/dep/lib.go",
	} {
		mustWrite(t, filepath.Join(dir, filepath.FromSlash(rel)), "x\n")
	}

	return dir
}

func globPaths(t *testing.T, dir, pattern string, limit int) GlobPage {
	t.Helper()

	page, err := GlobFiles(dir, pattern, limit)
	if err != nil {
		t.Fatalf("GlobFiles(%q): %v", pattern, err)
	}

	return page
}

// A pattern with no "/" is matched against the base name at any depth: that is
// what makes "*.sql" useful without knowing where migrations live.
func TestGlobFilesBareBasenamePatternMatchesAtAnyDepth(t *testing.T) {
	t.Parallel()

	dir := globRepo(t)

	page := globPaths(t, dir, "*.sql", 0)
	want := []string{"db/migrations/001_init.sql", "db/migrations/002_users.sql"}

	if !slices.Equal(page.Paths, want) {
		t.Fatalf("*.sql = %v, want %v", page.Paths, want)
	}

	if got := globPaths(t, dir, "Makefile", 0).Paths; !slices.Equal(got, []string{"Makefile", "internal/service/Makefile"}) {
		t.Fatalf("Makefile = %v, want both depths", got)
	}
}

// A pattern containing "/" is anchored at the repository root, so the same
// relative path nested deeper must not match.
func TestGlobFilesSlashPatternIsAnchoredAtRoot(t *testing.T) {
	t.Parallel()

	dir := globRepo(t)

	page := globPaths(t, dir, "_ui/src/**", 0)
	want := []string{"_ui/src/app.ts", "_ui/src/lib/store.ts"}

	if !slices.Equal(page.Paths, want) {
		t.Fatalf("_ui/src/** = %v, want %v", page.Paths, want)
	}

	// A trailing "/" is shorthand for the same subtree.
	if got := globPaths(t, dir, "_ui/src/", 0).Paths; !slices.Equal(got, want) {
		t.Fatalf("_ui/src/ = %v, want %v", got, want)
	}

	// A leading "/" is redundant, not a different (base-name) pattern.
	if got := globPaths(t, dir, "/main.go", 0).Paths; !slices.Equal(got, []string{"main.go"}) {
		t.Fatalf("/main.go = %v, want the root file only", got)
	}
}

func TestGlobFilesDoublestarSpansDepths(t *testing.T) {
	t.Parallel()

	dir := globRepo(t)

	if got := globPaths(t, dir, "**/Makefile", 0).Paths; !slices.Equal(got, []string{"Makefile", "internal/service/Makefile"}) {
		t.Fatalf("**/Makefile = %v, want root and nested", got)
	}

	if got := globPaths(t, dir, "**/_ui/src/*.ts", 0).Paths; !slices.Equal(got, []string{"_ui/src/app.ts", "nested/_ui/src/app.ts"}) {
		t.Fatalf("**/_ui/src/*.ts = %v, want both depths", got)
	}
}

// Truncation must be reported, and the kept window must be the lexicographic
// prefix so a truncated answer is stable across calls.
func TestGlobFilesLimitTruncatesAndReportsTheTotal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := range 25 {
		mustWrite(t, filepath.Join(dir, "pkg", fmt.Sprintf("f%02d.go", i)), "package p\n")
	}

	page := globPaths(t, dir, "pkg/*.go", 10)

	if len(page.Paths) != 10 || page.Limit != 10 || page.Total != 25 || !page.Truncated {
		t.Fatalf("truncated page = %+v", page)
	}

	if page.Paths[0] != "pkg/f00.go" || page.Paths[9] != "pkg/f09.go" {
		t.Fatalf("truncated window is not the lexicographic prefix: %v", page.Paths)
	}

	full := globPaths(t, dir, "pkg/*.go", 0)
	if full.Truncated || full.Total != 25 || len(full.Paths) != 25 {
		t.Fatalf("untruncated page = %+v", full)
	}
}

// Non-content dirs stay hidden: a glob must not surface .git objects or
// krabby's own graphify output. Vendored trees are content and MUST be
// matchable - a caller who writes "vendor/**" asked for exactly that, and an
// empty page would be a wrong answer.
func TestGlobFilesHidesArtefactsButNotVendoredCode(t *testing.T) {
	t.Parallel()

	dir := globRepo(t)

	for _, pattern := range []string{"**/*", "*.go", "config", "*.json"} {
		for _, p := range globPaths(t, dir, pattern, MaxGlobLimit).Paths {
			if p == ".git/config" || p == "graphify-out/graph.json" {
				t.Fatalf("pattern %q surfaced artefact path %s", pattern, p)
			}
		}
	}

	page := globPaths(t, dir, "vendor/**", MaxGlobLimit)
	if page.Total != 1 || len(page.Paths) != 1 || page.Paths[0] != "vendor/dep/lib.go" {
		t.Fatalf("vendor/** = %+v, want the one vendored file", page)
	}

	// The count is the other half of the answer: a pruned tree that still
	// contributed to Total would be a page whose own numbers disagree.
	all := globPaths(t, dir, "**/*.go", MaxGlobLimit)
	if !slices.Contains(all.Paths, "vendor/dep/lib.go") || all.Total != len(all.Paths) {
		t.Fatalf("**/*.go = %+v, want the vendored file counted and listed", all)
	}
}

// An empty result's hint names the repository's top-level directories, so it
// must name the ones a glob can actually reach. Asserting a layout the walk
// refuses to search is worse than no hint: it sends the caller looking for a
// pattern that cannot exist.
func TestGlobFilesHintNamesReachableDirectories(t *testing.T) {
	t.Parallel()

	page := globPaths(t, globRepo(t), "nothing/here/**", MaxGlobLimit)
	if page.Total != 0 {
		t.Fatalf("expected an empty page, got %+v", page)
	}

	if !strings.Contains(page.Hint, "vendor/") {
		t.Errorf("hint omits a directory a glob can search: %q", page.Hint)
	}

	if strings.Contains(page.Hint, ".git/") || strings.Contains(page.Hint, "graphify-out/") {
		t.Errorf("hint names a pruned artefact directory: %q", page.Hint)
	}
}

// A pattern is not a path, so ".." is refused rather than resolved: silently
// interpreting it is how a sandboxed reader starts naming files above the root.
func TestGlobFilesRejectsParentTraversalPattern(t *testing.T) {
	t.Parallel()

	dir := globRepo(t)

	for _, pattern := range []string{"../*.txt", "..", "_ui/../../secret.txt", "**/../*"} {
		if _, err := GlobFiles(dir, pattern, 0); err == nil {
			t.Fatalf("pattern %q must be refused", pattern)
		}
	}

	if _, err := GlobFiles(dir, "  ", 0); err == nil {
		t.Fatal("empty pattern must be refused")
	}
}

// An empty result is the case a caller most often misreads, so it carries the
// anchoring rule and the repository's real top level instead of a bare list.
func TestGlobFilesEmptyResultCarriesVocabulary(t *testing.T) {
	t.Parallel()

	dir := globRepo(t)

	page := globPaths(t, dir, "ui/src/**", 0)
	if page.Total != 0 || len(page.Paths) != 0 {
		t.Fatalf("expected no matches, got %+v", page)
	}

	if page.Hint == "" {
		t.Fatal("empty anchored result must explain itself")
	}

	for _, name := range []string{"_ui/", "db/", "internal/"} {
		if !strings.Contains(page.Hint, name) {
			t.Fatalf("hint %q omits top-level directory %s", page.Hint, name)
		}
	}

	bare := globPaths(t, dir, "*.rs", 0)
	if bare.Hint == "" {
		t.Fatalf("empty base-name result must explain itself: %+v", bare)
	}
}
