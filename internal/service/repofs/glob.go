package repofs

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// Bounds on one glob response. A glob can match an entire repository, so the
// answer is always a bounded prefix with an honest total beside it.
const (
	// DefaultGlobLimit is used when the caller names no limit.
	DefaultGlobLimit = 200
	// MaxGlobLimit is the hard ceiling on paths returned in one call.
	MaxGlobLimit = 2000
)

// GlobPage is one bounded page of matching paths.
type GlobPage struct {
	Paths []string `json:"paths"`
	Total int      `json:"total"`
	Limit int      `json:"limit"`
	// Truncated reports the walk stopped at Limit and more paths match: Total
	// counts every match, Paths holds the lexicographically first Limit of them.
	Truncated bool   `json:"truncated"`
	Snapshot  string `json:"snapshot,omitempty"`
	// Hint is set only when nothing matched. A zero-match glob is almost always
	// a misunderstood anchoring rule or a wrong top-level directory name, so the
	// empty result carries the rule and the repository's actual top level rather
	// than leaving the caller to guess twice.
	Hint string `json:"hint,omitempty"`
}

// GlobFiles returns the repo-relative slash paths of regular files matching
// pattern, sorted lexicographically so repeated calls and diffs are stable.
//
// Anchoring is the one thing a caller gets wrong, so the rule is fixed and
// explicit: a pattern with no "/" is matched against the base name at any depth
// ("*.sql" finds db/migrations/001.sql), and a pattern containing a "/" is
// anchored at the repository root ("_ui/src/**" can never match outside
// _ui/src). "**" spans path segments, so "**/Makefile" is how you ask for a
// base name anchored nowhere but written with a path. A trailing "/" is
// shorthand for the subtree: "_ui/src/" means "_ui/src/**".
//
// A pattern containing ".." is refused rather than interpreted. A pattern is
// not a path: path.Clean would silently rewrite "a/**/../b" into something the
// caller never asked for, and since matching happens against already-clean
// repo-relative paths no ".." segment could ever match anyway. The walk itself
// is confined by os.Root, as every other read in this package is.
//
// Everything in the clone is matchable except git's own object store and
// krabby's graphify output, neither of which is repository content. Vendored
// trees are matched: browsing hides them, but a caller who writes "vendor/**"
// has asked for them, and Total counts every match so the page's own numbers
// stay honest.
func GlobFiles(rootDir, pattern string, limit int) (GlobPage, error) {
	pat, anchored, err := NormalizeGlob(pattern)
	if err != nil {
		return GlobPage{}, err
	}

	if limit <= 0 {
		limit = DefaultGlobLimit
	}

	if limit > MaxGlobLimit {
		limit = MaxGlobLimit
	}

	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return GlobPage{}, fmt.Errorf("open repo root; %w", err)
	}
	defer func() { _ = root.Close() }()

	page := GlobPage{Paths: []string{}, Limit: limit}
	matcher := CompileGlob(pat)

	walkFn := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries, as listings do
		}

		if p == "." {
			return nil
		}

		if d.IsDir() {
			// Only non-content directories are pruned: a glob names paths, so
			// hiding a real subtree from it would answer a different question
			// than the one asked. A subtree that cannot contain a match is
			// pruned too, so an anchored pattern costs its own subtree rather
			// than the repository.
			if globSkipDirs[path.Base(p)] || (anchored && !globCanDescend(pat, p)) {
				return fs.SkipDir
			}

			return nil
		}

		// Symlinks are skipped rather than followed: a link out of the clone
		// must not be able to name a path outside it.
		if !d.Type().IsRegular() {
			return nil
		}

		if !globMatches(matcher, anchored, p) {
			return nil
		}

		page.Total++
		insertBounded(&page.Paths, p, limit)

		return nil
	}

	if err := fs.WalkDir(root.FS(), ".", walkFn); err != nil {
		return GlobPage{}, fmt.Errorf("glob %s; %w", pattern, err)
	}

	page.Truncated = page.Total > limit
	if page.Total == 0 {
		page.Hint = globHint(root, pat, anchored)
	}

	return page, nil
}

// NormalizeGlob applies the shared shape rules to a user-supplied glob pattern
// and reports whether it is anchored at the repository root (see GlobFiles for
// the rule). It is exported because coderag scopes a search with the same kind
// of pattern: a trailing "/" and a rejected ".." have to mean the same thing in
// a glob call and in a path-filtered search, and they diverged while each
// caller kept its own copy of these five lines.
func NormalizeGlob(pattern string) (string, bool, error) {
	pat := strings.TrimSpace(pattern)
	if pat == "" {
		return "", false, fmt.Errorf(`glob pattern is empty; try "*.sql" (base name, any depth), "**/Makefile" or "_ui/src/**" (anchored at the repo root)`)
	}

	// Anchoring is decided on the pattern as written, before a redundant
	// leading "/" is dropped, so "/cmd/main.go" stays root-anchored instead of
	// degrading into a base-name match.
	anchored := strings.Contains(pat, "/")
	pat = strings.TrimPrefix(pat, "/")

	if strings.HasSuffix(pat, "/") {
		pat += "**"
	}

	if pat == "" || pat == "." {
		// "" and "." name the repository root itself, which is a directory, not
		// a file, so no file path can ever match them.
		return "", false, fmt.Errorf("glob pattern %q names the repository root, not files; use list_files or a pattern like \"**/*.go\"", pattern)
	}

	for _, seg := range strings.Split(pat, "/") {
		if seg == ".." {
			return "", false, fmt.Errorf(`glob pattern %q must not contain ".."; patterns are matched against repository-relative paths, so there is nothing above the root to name`, pattern)
		}
	}

	return pat, anchored, nil
}

// globMatches applies the anchoring rule to one path.
func globMatches(m GlobMatcher, anchored bool, rel string) bool {
	if anchored {
		return m.Match(rel)
	}

	return m.Match(path.Base(rel))
}

// globHint explains an empty result. For an anchored pattern the usual cause is
// a wrong first segment, so the repository's top-level directories are the
// vocabulary worth handing back.
func globHint(root *os.Root, pat string, anchored bool) string {
	if !anchored {
		return fmt.Sprintf("no file's base name matches %q at any depth; a pattern containing \"/\" is anchored at the repo root instead", pat)
	}

	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Sprintf("nothing matches %q, which is anchored at the repository root", pat)
	}

	var dirs []string

	for _, e := range entries {
		if e.IsDir() && !globSkipDirs[e.Name()] {
			dirs = append(dirs, e.Name()+"/")
		}
	}

	slices.Sort(dirs)

	if len(dirs) > 40 {
		dirs = dirs[:40]
	}

	top := "(no directories at the repository root)"
	if len(dirs) > 0 {
		top = strings.Join(dirs, ", ")
	}

	return fmt.Sprintf("nothing matches %q, which is anchored at the repository root. Top-level directories: %s", pat, top)
}

// insertBounded keeps paths sorted and no longer than limit, so a glob over a
// huge match set costs O(limit) memory while Total still counts every match.
// The kept window is the lexicographically smallest limit paths, which is what
// makes the truncated answer deterministic rather than walk-order dependent.
func insertBounded(paths *[]string, p string, limit int) {
	at, _ := slices.BinarySearch(*paths, p)
	if at >= limit {
		return
	}

	if len(*paths) == limit {
		*paths = (*paths)[:limit-1]
	}

	*paths = slices.Insert(*paths, at, p)
}
