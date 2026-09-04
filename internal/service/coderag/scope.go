package coderag

import (
	"fmt"
	"strings"

	"github.com/rytsh/krabby/internal/service/repofs"
)

// Scoping a code search is one operation for every mode, because everything it
// needs is already in the chunk id: `<repo>/<path>#<n>`. Both the repository
// scope and a path filter are therefore string tests on a key the ranking (or
// the trigram prefilter) has already produced, costing a function call per hit
// instead of a point read and a decode.
//
// Path filtering is a glob rather than a query operator. A structured argument
// cannot be mis-escaped, needs no grammar for a caller to learn, and reads the
// same from an MCP client and from the web UI's form — where a `path:` prefix
// inside the query string would have to be parsed out of the same box the user
// types identifiers into.

// Scope selects which chunks a search may return.
type Scope struct {
	// Repos are the repository ids in scope. Empty means every repository,
	// which is only usable when Path is empty or a bare-basename pattern:
	// anchoring a path pattern requires knowing where the repository prefix
	// ends, and a repository id contains slashes of its own.
	Repos []string
	// Path is an optional glob over the repo-relative source path. A pattern
	// without a slash matches the file's base name at any depth (`*_test.go`);
	// a pattern with one is anchored at the repository root (`internal/**`).
	Path string
}

// KeyFilter compiles the scope into a chunk-id predicate. A nil result means
// "everything", which callers pass straight to bw as an absent filter.
func (s Scope) KeyFilter() (func(string) bool, error) {
	path, anchored, err := s.pathPattern()
	if err != nil {
		return nil, err
	}

	prefixes := make([]string, 0, len(s.Repos))
	for _, repo := range s.Repos {
		prefixes = append(prefixes, repo+"/")
	}

	// The pattern is compiled once here rather than on every hit: a filter is
	// called for each candidate the ranking produced, which is the whole
	// point of scoping on the key instead of on a decoded record.
	matcher := repofs.CompileGlob(path)

	switch {
	case path == "" && len(prefixes) == 0:
		return nil, nil

	case path == "":
		return func(id string) bool { return matchAnyPrefix(id, prefixes) != "" }, nil

	case !anchored:
		// The base name sits between the last slash and the chunk suffix, so a
		// bare pattern needs no repository prefix to be resolved against.
		return func(id string) bool {
			if len(prefixes) > 0 && matchAnyPrefix(id, prefixes) == "" {
				return false
			}

			return matcher.Match(baseName(chunkPath(id)))
		}, nil

	default:
		return func(id string) bool {
			prefix := matchAnyPrefix(id, prefixes)
			if prefix == "" {
				return false
			}

			return matcher.Match(chunkPath(id[len(prefix):]))
		}, nil
	}
}

// PathMatcher compiles the scope's path pattern into a predicate over a
// repo-relative source path. A nil result means "no path filter".
//
// It exists because the semantic mode has the path in hand — a vector hit
// carries its repository and path as separate fields — so it needs no
// repository prefix to strip and no key to take apart. The anchoring rule is
// the same one KeyFilter applies, and it is applied here by the same matcher,
// so a pattern cannot mean one thing in one mode and another in the next.
func (s Scope) PathMatcher() (func(string) bool, error) {
	path, anchored, err := s.pathPattern()
	if err != nil || path == "" {
		return nil, err
	}

	matcher := repofs.CompileGlob(path)
	if anchored {
		return matcher.Match, nil
	}

	return func(rel string) bool { return matcher.Match(baseName(rel)) }, nil
}

// pathPattern normalises Scope.Path and reports whether it is anchored. An
// empty pattern is not an error here - it means "no path filter" - so the
// shared rules are applied only to a pattern the caller actually wrote.
//
// The anchored-pattern-needs-a-repository rule lives here rather than in
// repofs because it is not a property of the pattern: it is a property of a
// chunk id, whose repository prefix has to be known before what follows it can
// be called a repo-relative path.
func (s Scope) pathPattern() (string, bool, error) {
	if strings.TrimSpace(s.Path) == "" {
		return "", false, nil
	}

	pat, anchored, err := repofs.NormalizeGlob(s.Path)
	if err != nil {
		return "", false, err
	}

	if anchored && len(s.Repos) == 0 {
		return "", false, fmt.Errorf("path pattern %q is anchored at the repository root, so the search must name a repo or a namespace; use a pattern without '/' to match a file name at any depth", pat)
	}

	return pat, anchored, nil
}

// matchAnyPrefix returns the matching repository prefix, or "" when the id
// belongs to none of them.
//
// The trailing separator is what stops "acme/app" from claiming
// "acme/apple"'s chunks. The longest match wins because a repository id is a
// path and one tracked repository can sit inside another's: resolving
// "acme/app/sub/cmd/x.go" against the shorter prefix would leave the path as
// "sub/cmd/x.go" and silently miss an anchored pattern.
func matchAnyPrefix(id string, prefixes []string) string {
	best := ""
	for _, prefix := range prefixes {
		if len(prefix) > len(best) && strings.HasPrefix(id, prefix) {
			best = prefix
		}
	}

	return best
}

// chunkPath strips the "#<n>" chunk suffix from an id fragment. The separator
// cannot appear in a path segment krabby indexes, and taking the last one keeps
// a file whose name contains '#' resolvable.
func chunkPath(s string) string {
	if i := strings.LastIndexByte(s, '#'); i >= 0 {
		return s[:i]
	}

	return s
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}

	return path
}
