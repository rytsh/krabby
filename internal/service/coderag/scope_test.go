package coderag

import "testing"

// TestScopeKeyFilter pins the chunk-id predicate every search mode shares.
// Getting it wrong is not a visible failure — it silently widens or narrows a
// search — so each rule has a row.
func TestScopeKeyFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		scope Scope
		match map[string]bool
	}{
		{
			name:  "unscoped matches everything",
			scope: Scope{},
			match: map[string]bool{"acme/app/main.go#0": true, "other/lib/x.go#3": true},
		},
		{
			name:  "repo prefix needs the separator",
			scope: Scope{Repos: []string{"acme/app"}},
			match: map[string]bool{
				"acme/app/main.go#0": true,
				// Without the trailing slash "acme/app" would claim these.
				"acme/apple/main.go#0": false,
				"other/lib/x.go#0":     false,
			},
		},
		{
			name:  "several repos",
			scope: Scope{Repos: []string{"acme/app", "acme/web"}},
			match: map[string]bool{
				"acme/app/main.go#0": true,
				"acme/web/main.go#0": true,
				"other/lib/x.go#0":   false,
			},
		},
		{
			name:  "bare pattern matches the base name at any depth",
			scope: Scope{Path: "*_test.go"},
			match: map[string]bool{
				"acme/app/main_test.go#0":                  true,
				"acme/app/internal/deep/nested_test.go#12": true,
				"acme/app/main.go#0":                       false,
			},
		},
		{
			name:  "bare pattern still honours the repo scope",
			scope: Scope{Repos: []string{"acme/app"}, Path: "*.go"},
			match: map[string]bool{
				"acme/app/main.go#0":   true,
				"other/lib/main.go#0":  false,
				"acme/app/README.md#0": false,
			},
		},
		{
			name:  "slashed pattern is anchored at the repository root",
			scope: Scope{Repos: []string{"acme/app"}, Path: "internal/**"},
			match: map[string]bool{
				"acme/app/internal/service/x.go#0": true,
				"acme/app/internal/y.go#0":         true,
				// Anchored, so a matching segment deeper in the tree is not a
				// match — that is the whole difference from the bare form.
				"acme/app/pkg/internal/z.go#0": false,
				"acme/app/main.go#0":           false,
			},
		},
		{
			name:  "anchored pattern strips the right repository prefix",
			scope: Scope{Repos: []string{"acme/app", "acme/app/sub"}, Path: "cmd/*.go"},
			match: map[string]bool{
				"acme/app/cmd/main.go#0": true,
				// The id belongs to the longer repo id, whose own cmd/ is a
				// different directory; matching the shorter prefix first would
				// resolve the path as "sub/cmd/main.go" and miss.
				"acme/app/sub/cmd/main.go#0": true,
			},
		},
		{
			name:  "chunk suffix is not part of the path",
			scope: Scope{Repos: []string{"r"}, Path: "a/b.go"},
			match: map[string]bool{"r/a/b.go#41": true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			filter, err := tc.scope.KeyFilter()
			if err != nil {
				t.Fatalf("KeyFilter: %v", err)
			}
			for id, want := range tc.match {
				got := filter == nil || filter(id)
				if got != want {
					t.Errorf("filter(%q) = %v, want %v", id, got, want)
				}
			}
		})
	}
}

// TestScopeKeyFilterRejects checks the two shapes that cannot be answered are
// named errors rather than a quietly wrong scope.
func TestScopeKeyFilterRejects(t *testing.T) {
	t.Parallel()

	// An anchored pattern needs to know where the repository prefix ends, and
	// a repository id contains slashes of its own, so it cannot be guessed.
	if _, err := (Scope{Path: "internal/**"}).KeyFilter(); err == nil {
		t.Error("anchored pattern without a repository scope was accepted")
	}
	if _, err := (Scope{Repos: []string{"r"}, Path: "../etc/passwd"}).KeyFilter(); err == nil {
		t.Error("pattern containing '..' was accepted")
	}
}

// TestScopeUnscopedFilterIsNil checks the unconstrained case costs nothing:
// bw is handed no filter at all rather than a predicate that always returns
// true and runs once per hit.
func TestScopeUnscopedFilterIsNil(t *testing.T) {
	t.Parallel()

	filter, err := (Scope{}).KeyFilter()
	if err != nil {
		t.Fatal(err)
	}
	if filter != nil {
		t.Error("unscoped search built a filter")
	}
}

// A path pattern is the same argument in `glob` and in a path-filtered search,
// so the two MUST read it the same way. These three rows diverged while each
// side kept its own copy of the normalisation, and each divergence is silent:
// the search returns a plausible, wrong result set.
func TestScopePatternMatchesGlobNormalisation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		path  string
		match map[string]bool
	}{
		{
			// "internal/" is the subtree shorthand glob accepts. Taken
			// literally it is a directory name, which no file path equals, so
			// the filter matched nothing at all.
			name:  "trailing slash means the subtree",
			path:  "internal/",
			match: map[string]bool{"r/internal/a/b.go#0": true, "r/cmd/main.go#0": false},
		},
		{
			// A redundant leading slash is dropped, not treated as a first
			// empty segment that can never match.
			name:  "leading slash is redundant",
			path:  "/cmd/*.go",
			match: map[string]bool{"r/cmd/main.go#0": true, "r/internal/a/b.go#0": false},
		},
		{
			// ".." is refused as a segment. A file whose name merely contains
			// two dots is a legitimate target and used to be refused with it.
			name:  "two dots inside a name are not traversal",
			path:  "*..bak",
			match: map[string]bool{"r/cmd/main..bak#0": true, "r/cmd/main.go#0": false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			filter, err := (Scope{Repos: []string{"r"}, Path: tc.path}).KeyFilter()
			if err != nil {
				t.Fatalf("KeyFilter(%q): %v", tc.path, err)
			}
			if filter == nil {
				t.Fatalf("KeyFilter(%q) built no filter", tc.path)
			}

			for id, want := range tc.match {
				if got := filter(id); got != want {
					t.Errorf("filter(%q) = %v, want %v", id, got, want)
				}
			}
		})
	}
}

// The semantic mode filters on a path it already holds instead of on a chunk
// id, so it is a second implementation of the same rule. It must agree with
// the first one, or a pattern means different things in different modes.
func TestScopePathMatcherAgreesWithKeyFilter(t *testing.T) {
	t.Parallel()

	paths := []string{"internal/a/b.go", "cmd/main.go", "main..bak"}

	for _, pattern := range []string{"internal/", "/cmd/*.go", "*..bak", "*.go", "**/*_test.go"} {
		scope := Scope{Repos: []string{"r"}, Path: pattern}

		key, err := scope.KeyFilter()
		if err != nil {
			t.Fatalf("KeyFilter(%q): %v", pattern, err)
		}

		match, err := scope.PathMatcher()
		if err != nil {
			t.Fatalf("PathMatcher(%q): %v", pattern, err)
		}
		if match == nil {
			t.Fatalf("PathMatcher(%q) built no matcher", pattern)
		}

		for _, p := range paths {
			if got, want := match(p), key("r/"+p+"#0"); got != want {
				t.Errorf("pattern %q: PathMatcher(%q) = %v, KeyFilter = %v", pattern, p, got, want)
			}
		}
	}
}
