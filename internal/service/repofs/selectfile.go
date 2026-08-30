package repofs

import (
	"path"
	"strings"
)

// This file owns the shared answer to "which files in a repository are source
// files, and does this path match a user glob?".
//
// It lives here for the same reason DeployConfigFile does: the code indexer
// (internal/service/coderag) and the doc generator (internal/service/docgen)
// both consume the same user-facing config.Filters, and both used to carry
// their own copy of this logic. The copies drifted — docgen matched globs with
// plain path.Match while coderag supported "**", so an exclude like
// "src/**/gen/*.go" dropped files from the code index and was silently ignored
// when generating docs. The two allowlists had also diverged, leaving Svelte,
// Vue, Lua, Zig and Elixir repositories indexed but undocumentable.
//
// Filters are passed as plain string slices rather than config.Filters so this
// package stays free of a dependency on internal/config.

// MatchAny reports whether rel matches any of the globs.
//
// A glob is matched against both the full path and the base name, "**" spans
// path segments, and a bare directory prefix (e.g. "vendor/") matches
// everything beneath it.
func MatchAny(globs []string, rel string) bool {
	base := path.Base(rel)
	for _, gl := range globs {
		if gl == "" {
			continue
		}

		if strings.HasSuffix(gl, "/") && strings.HasPrefix(rel, gl) {
			return true
		}

		if globMatch(gl, rel) {
			return true
		}

		if ok, _ := path.Match(gl, base); ok {
			return true
		}
	}

	return false
}

// globMatch implements slash-aware glob matching with doublestar support. Each
// ordinary segment uses path.Match; a "**" segment consumes zero or more path
// segments.
func globMatch(pattern, name string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	nameParts := strings.Split(strings.Trim(name, "/"), "/")

	type state struct{ pattern, name int }
	memo := map[state]bool{}
	seen := map[state]bool{}

	var match func(int, int) bool
	match = func(pi, ni int) bool {
		st := state{pi, ni}
		if seen[st] {
			return memo[st]
		}
		seen[st] = true

		var ok bool
		switch {
		case pi == len(patternParts):
			ok = ni == len(nameParts)
		case patternParts[pi] == "**":
			ok = match(pi+1, ni) || (ni < len(nameParts) && match(pi, ni+1))
		case ni < len(nameParts):
			segmentOK, _ := path.Match(patternParts[pi], nameParts[ni])
			ok = segmentOK && match(pi+1, ni+1)
		}

		memo[st] = ok

		return ok
	}

	return match(0, 0)
}

// MatchInclude reports whether rel is selected by the include filters.
//
// includeExtra is checked first because it is purely additive: it widens
// whatever include resolved to, so a repository can opt one more family of
// files in without restating the allowlist it was happy with. An empty include
// falls back to the built-in source-file allowlist.
func MatchInclude(rel string, include, includeExtra []string) bool {
	if MatchAny(includeExtra, rel) {
		return true
	}

	if len(include) > 0 {
		return MatchAny(include, rel)
	}

	return SourceFile(rel)
}

// SourceFile applies the built-in allowlist used when no include globs are
// configured: a source extension, a known build-config file name, or a
// deployment/CI config file.
//
// Deploy config counts as source here because which service runs which image
// version per environment is part of understanding a system, and for a
// deployment-only repository it is the only thing present — without it such a
// repo would index and document nothing at all.
func SourceFile(rel string) bool {
	rel = strings.ToLower(rel)

	return sourceExts[path.Ext(rel)] ||
		sourceNames[path.Base(rel)] ||
		DeployConfigFile(rel)
}

// sourceExts is the source-file extension allowlist.
var sourceExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".java": true, ".kt": true, ".rb": true, ".rs": true, ".c": true, ".h": true,
	".cc": true, ".cpp": true, ".hpp": true, ".cs": true, ".php": true, ".swift": true,
	".scala": true, ".m": true, ".mm": true, ".sh": true, ".sql": true, ".svelte": true,
	".vue": true, ".lua": true, ".zig": true, ".ex": true, ".exs": true,
}

// sourceNames is the allowlist of extensionless or dotted-suffix source files,
// matched by base name (lowercased). path.Ext does not classify these usefully
// — path.Ext("go.mod") is ".mod" — so they would otherwise be skipped, hiding
// dependency versions and build config.
var sourceNames = map[string]bool{
	"go.mod":           true,
	"go.sum":           true,
	"dockerfile":       true,
	"makefile":         true,
	"gemfile":          true,
	"rakefile":         true,
	"cargo.toml":       true,
	"cargo.lock":       true,
	"package.json":     true,
	"pyproject.toml":   true,
	"requirements.txt": true,
}
