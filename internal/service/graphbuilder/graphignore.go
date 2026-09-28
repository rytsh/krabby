package graphbuilder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultGraphIgnore lists gitignore-style patterns krabby always excludes from
// the knowledge graph. These are test fixtures, generated assets and other
// files that carry no architectural meaning but would otherwise flood the graph
// with nodes (for example parsed JSON fixtures under testdata/). bag's own
// built-in skip list already covers dependency and build dirs (node_modules,
// dist, __pycache__, ...), so this list only adds what it misses.
var DefaultGraphIgnore = []string{
	// Test fixtures and sample data.
	"testdata/",
	"test-data/",
	"fixtures/",
	"__fixtures__/",
	"testfixtures/",
	"mocks/",
	"__mocks__/",
	// Vendored dependency trees not covered by bag's built-in skip list
	// (Go's vendor/ can hold thousands of third-party source files that stall
	// the graph build).
	"vendor/",
	// Generated / vendored assets that are not source.
	"*.min.js",
	"*.min.css",
	"*.map",
	"*.pb.go",
	"*_pb2.py",
	"*.generated.*",
	"*.gen.go",
	// Large data blobs occasionally committed alongside code.
	"*.snap",
}

const (
	// legacyIgnoreFileName was written before Krabby passed excludes directly to
	// bag. It remains here only so the managed block can be removed.
	legacyIgnoreFileName = ".graphifyignore"

	// managedBegin/managedEnd delimit the block krabby owns inside the file.
	// Anything outside the markers (a user's own patterns) is preserved.
	managedBegin = "# >>> krabby managed (do not edit) >>>"
	managedEnd   = "# <<< krabby managed <<<"
)

// RemoveLegacyManagedIgnore removes only the block older Krabby versions added
// to .graphifyignore. User-authored content outside the markers is preserved.
// If the generated block was the file's only content, the file is removed.
func RemoveLegacyManagedIgnore(clonePath string) (changed bool, err error) {
	path := filepath.Join(clonePath, legacyIgnoreFileName)
	existing, err := os.ReadFile(path) //nolint:gosec // path is a tracked clone root
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s; %w", legacyIgnoreFileName, err)
	}
	if !strings.Contains(string(existing), managedBegin) {
		return false, nil
	}

	preserved := stripManagedBlock(string(existing))
	if strings.TrimSpace(preserved) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("remove %s; %w", legacyIgnoreFileName, err)
		}
		return true, nil
	}

	preserved = strings.TrimRight(preserved, "\n") + "\n"
	if err := os.WriteFile(path, []byte(preserved), 0o644); err != nil { //nolint:gosec // ignore file is non-secret
		return false, fmt.Errorf("rewrite %s; %w", legacyIgnoreFileName, err)
	}

	return true, nil
}

// GraphHasExcludedNodes reports whether the built graph at clonePath still
// contains nodes whose source_file matches the current exclude rules (defaults +
// extra). It lets the refresh path rebuild a stale graph even when git did not
// change — otherwise a graph built before the ignore rules existed would keep
// its testdata/fixture nodes forever. Missing/unreadable graphs return false.
func GraphHasExcludedNodes(clonePath string, extra []string) bool {
	b, err := os.ReadFile(GraphPath(clonePath)) //nolint:gosec // clone-derived path
	if err != nil {
		return false
	}

	var g struct {
		Nodes []struct {
			SourceFile string `json:"source_file"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return false
	}

	patterns := mergedPatterns(extra)
	for _, n := range g.Nodes {
		if n.SourceFile != "" && matchesExcluded(n.SourceFile, patterns) {
			return true
		}
	}

	return false
}

// mergedPatterns returns the deduplicated default + extra exclude patterns,
// normalized (no surrounding slashes) for segment matching.
func mergedPatterns(extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(append([]string{}, DefaultGraphIgnore...), extra...) {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}

	return out
}

// matchesExcluded reports whether a repo-relative slash path matches any exclude
// pattern, mirroring bag's gitignore semantics: a bare pattern matches at
// any path depth (each segment and each cumulative prefix is tested), so
// "testdata" matches "a/b/testdata/c.json".
func matchesExcluded(rel string, patterns []string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	parts := strings.Split(rel, "/")
	base := parts[len(parts)-1]

	for _, p := range patterns {
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
		for i, seg := range parts {
			if ok, _ := filepath.Match(p, seg); ok {
				return true
			}
			if ok, _ := filepath.Match(p, strings.Join(parts[:i+1], "/")); ok {
				return true
			}
		}
	}

	return false
}

// stripManagedBlock removes a previously written managed block (and its
// surrounding blank lines) from content, returning only the user's own lines.
func stripManagedBlock(content string) string {
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")

	var (
		out      []string
		inBlock  bool
		sawBlock bool
	)

	for _, line := range lines {
		switch {
		case strings.TrimSpace(line) == managedBegin:
			inBlock = true
			sawBlock = true

			continue
		case strings.TrimSpace(line) == managedEnd:
			inBlock = false

			continue
		}

		if !inBlock {
			out = append(out, line)
		}
	}

	preserved := strings.Join(out, "\n")

	// When we removed a managed block, trim the trailing blank lines it left so
	// the rewrite does not accumulate empty lines across runs.
	if sawBlock {
		preserved = strings.TrimRight(preserved, "\n")
	}

	return preserved
}

// MergeExclude unions install-wide ignore patterns with one repository's, in
// that order, without aliasing either input. It mirrors what a build applies,
// so callers surfacing the effective configuration cannot drift from it.
func MergeExclude(global, repo []string) []string {
	if len(repo) == 0 {
		return global
	}
	if len(global) == 0 {
		return repo
	}

	out := make([]string, 0, len(global)+len(repo))
	out = append(out, global...)

	return append(out, repo...)
}
