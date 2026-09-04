package repofs

import "testing"

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
		if got := MatchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

// globCanDescend decides whether a glob walk may prune a subtree. A wrong
// "false" loses matches silently, so both directions are pinned here rather
// than only through GlobFiles' results.
func TestGlobCanDescendPrunesOnlyUnreachableSubtrees(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern string
		dir     string
		want    bool
	}{
		{"_ui/src/**", "_ui", true},
		{"_ui/src/**", "_ui/src", true},
		{"_ui/src/**", "_ui/src/lib", true},
		{"_ui/src/**", "internal", false},
		{"_ui/src/**", "_ui/build", false},
		{"**/Makefile", "internal/service", true},
		{"internal/*/*.go", "internal/service", true},
		{"internal/*/*.go", "internal/service/repofs", false},
		{"cmd/main.go", "cmd", true},
		// The pattern is fully consumed by the directory path itself, so no
		// file below it can match.
		{"cmd/main.go", "cmd/main.go", false},
		{"cmd/*/main.go", "cmd", true},
	}

	for _, tt := range tests {
		if got := globCanDescend(tt.pattern, tt.dir); got != tt.want {
			t.Errorf("globCanDescend(%q, %q) = %v, want %v", tt.pattern, tt.dir, got, tt.want)
		}
	}
}

// TestMatchAnySpansSegments pins the behaviour that used to differ between the
// code indexer and the doc generator: docgen matched with plain path.Match, so
// a "**" exclude silently did nothing there while working in coderag.
func TestMatchAnySpansSegments(t *testing.T) {
	t.Parallel()

	globs := []string{"src/**/gen/*.go"}
	if !MatchAny(globs, "src/a/b/gen/api.go") {
		t.Error("MatchAny should span path segments through **")
	}
	if MatchAny(globs, "src/a/b/handwritten.go") {
		t.Error("MatchAny matched a path the glob does not cover")
	}
}

func TestMatchAnyDirectoryPrefixAndBaseName(t *testing.T) {
	t.Parallel()

	if !MatchAny([]string{"vendor/"}, "vendor/x/y.go") {
		t.Error("a bare directory prefix should match everything beneath it")
	}
	if !MatchAny([]string{"*.md"}, "docs/deep/README.md") {
		t.Error("a glob should also match the base name")
	}
	if MatchAny([]string{""}, "anything.go") {
		t.Error("an empty glob must never match")
	}
}

// TestSourceFileCoversAllIndexedLanguages guards the second half of the drift:
// the two allowlists had diverged, so Svelte/Vue/Lua/Zig/Elixir repositories
// were code-indexed but produced "no source files to document".
func TestSourceFileCoversAllIndexedLanguages(t *testing.T) {
	t.Parallel()

	for _, rel := range []string{
		"app/Component.svelte", "app/Widget.vue", "cfg/init.lua",
		"src/main.zig", "lib/app.ex", "test/app_test.exs",
		"cmd/main.go", "svc/handler.py", "web/index.ts",
	} {
		if !SourceFile(rel) {
			t.Errorf("SourceFile(%q) = false, want true", rel)
		}
	}

	for _, rel := range []string{"README.md", "logo.png", "notes.txt"} {
		if SourceFile(rel) {
			t.Errorf("SourceFile(%q) = true, want false", rel)
		}
	}
}

func TestSourceFileMatchesBuildConfigByName(t *testing.T) {
	t.Parallel()

	for _, rel := range []string{
		"go.mod", "sub/go.sum", "Dockerfile", "Makefile",
		"Cargo.toml", "ui/package.json", "requirements.txt",
	} {
		if !SourceFile(rel) {
			t.Errorf("SourceFile(%q) = false, want true (build config)", rel)
		}
	}
}

func TestMatchIncludeExtraIsAdditive(t *testing.T) {
	t.Parallel()

	include := []string{"*.go"}

	if MatchInclude("docs/guide.md", include, nil) {
		t.Error("a non-matching path must not be included")
	}
	if !MatchInclude("docs/guide.md", include, []string{"docs/**"}) {
		t.Error("IncludeExtra must widen an existing Include")
	}
	// A non-empty Include replaces the built-in allowlist.
	if MatchInclude("main.py", include, nil) {
		t.Error("an explicit Include should not fall back to the default allowlist")
	}
	// An empty Include falls back to the built-in allowlist.
	if !MatchInclude("main.py", nil, nil) {
		t.Error("an empty Include should use the default source allowlist")
	}
}
