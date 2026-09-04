package repofs

import (
	"path"
	"slices"
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
// MatchGlob is that one matcher. GlobFiles (path-pattern file lookup) and
// coderag's path scoping call it as well, so a pattern means the same thing in
// a config filter, a glob tool call and a scoped search.
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

		if MatchGlob(gl, rel) {
			return true
		}

		if ok, _ := path.Match(gl, base); ok {
			return true
		}
	}

	return false
}

// GlobMatcher is one glob compiled for repeated matching. A glob walk tests
// every file in a repository against the same pattern, so the pattern is split
// once here rather than on every path.
type GlobMatcher struct {
	parts []string
	// stars records whether any segment is "**". Without one the match is a
	// straight segment-by-segment comparison of equal-length paths, which is
	// the common case and needs no table.
	stars bool
}

// CompileGlob prepares pattern for matching. See MatchGlob for the semantics;
// the two are the same matcher, and CompileGlob is what to reach for when more
// than one path will be tested.
func CompileGlob(pattern string) GlobMatcher {
	parts := strings.Split(strings.Trim(pattern, "/"), "/")

	return GlobMatcher{parts: parts, stars: slices.Contains(parts, "**")}
}

// Match reports whether name matches the compiled glob.
func (m GlobMatcher) Match(name string) bool {
	if len(m.parts) == 0 {
		return false
	}

	nameParts := strings.Split(strings.Trim(name, "/"), "/")

	if !m.stars {
		if len(nameParts) != len(m.parts) {
			return false
		}

		for i, p := range m.parts {
			if ok, _ := path.Match(p, nameParts[i]); !ok {
				return false
			}
		}

		return true
	}

	return m.matchStars(nameParts)
}

// matchStars decides a pattern containing "**" with a table over
// (pattern suffix, path suffix) pairs: cell (pi, ni) is "pattern from segment
// pi matches path from segment ni". A "**" segment can consume nothing or one
// more segment, which is what makes the pair space a lattice rather than a
// walk, and the table is what keeps it linear in its size instead of
// exponential. One slice, filled backwards from the empty/empty match.
func (m GlobMatcher) matchStars(nameParts []string) bool {
	w := len(nameParts) + 1
	table := make([]bool, (len(m.parts)+1)*w)
	table[len(m.parts)*w+len(nameParts)] = true

	for pi := len(m.parts) - 1; pi >= 0; pi-- {
		for ni := len(nameParts); ni >= 0; ni-- {
			var ok bool

			switch {
			case m.parts[pi] == "**":
				// Skip the "**", or let it swallow nameParts[ni]; the
				// latter cell is already filled because ni descends.
				ok = table[(pi+1)*w+ni] || (ni < len(nameParts) && table[pi*w+ni+1])
			case ni < len(nameParts):
				segmentOK, _ := path.Match(m.parts[pi], nameParts[ni])
				ok = segmentOK && table[(pi+1)*w+ni+1]
			}

			table[pi*w+ni] = ok
		}
	}

	return table[0]
}

// MatchGlob reports whether one slash-aware glob matches a path. Each ordinary
// segment is matched with path.Match; a "**" segment consumes zero or more path
// segments. The pattern is matched against the whole path, so it is anchored:
// deciding whether a bare "*.go" should also match a base name at any depth is
// the caller's job (GlobFiles and coderag's path scoping each answer it
// differently), and folding that choice in here would make an anchored pattern
// silently match everywhere.
//
// It is exported so that every caller shares this one implementation; the
// duplicated copies described above drifted once already. Use CompileGlob when
// the same pattern is tested against many paths.
func MatchGlob(pattern, name string) bool {
	return CompileGlob(pattern).Match(name)
}

// globCanDescend reports whether any path below dir could still match pattern.
// It is the prefix-relaxed twin of MatchGlob and exists so a walk can prune a
// whole subtree instead of reading it: dir must match the pattern's leading
// segments, and at least one pattern segment must be left over to match
// something deeper. A "**" segment absorbs any remainder, so it always permits
// descent.
//
// It answers only for anchored patterns. A pattern matched against base names
// can hit at any depth, so for those nothing is prunable and callers must not
// consult this.
func globCanDescend(pattern, dir string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	dirParts := strings.Split(strings.Trim(dir, "/"), "/")

	for di, seg := range dirParts {
		if di >= len(patternParts) {
			// The pattern is fully consumed by the directory path itself, so
			// nothing deeper can match it.
			return false
		}

		if patternParts[di] == "**" {
			return true
		}

		if ok, _ := path.Match(patternParts[di], seg); !ok {
			return false
		}
	}

	return len(dirParts) < len(patternParts)
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
