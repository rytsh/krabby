package graphify

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
// with nodes (for example parsed JSON fixtures under testdata/). graphify's own
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
	// ignoreFileName is the file graphify reads for exclusion patterns. It sits
	// at the clone root so graphify's VCS-bounded resolver (which stops at .git)
	// always finds it.
	ignoreFileName = ".graphifyignore"

	// managedBegin/managedEnd delimit the block krabby owns inside the file.
	// Anything outside the markers (a user's own patterns) is preserved.
	managedBegin = "# >>> krabby managed (do not edit) >>>"
	managedEnd   = "# <<< krabby managed <<<"
)

// WriteIgnore refreshes the krabby-managed block in the clone's .graphifyignore
// so the next graph build skips DefaultGraphIgnore plus the given extra
// patterns. Any content the user placed outside the managed markers is kept
// verbatim. When the resulting managed block matches what is already on disk,
// the file is left untouched and changed is false. changed is true when the
// file was created or rewritten, signalling the caller to force a full graph
// rebuild (the new exclusions shrink the node count, which graphify otherwise
// refuses to overwrite).
func WriteIgnore(clonePath string, extra []string) (changed bool, err error) {
	path := filepath.Join(clonePath, ignoreFileName)

	existing, err := os.ReadFile(path) //nolint:gosec // path is a tracked clone root
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s; %w", ignoreFileName, err)
	}

	preserved := stripManagedBlock(string(existing))
	block := buildManagedBlock(extra)

	var b strings.Builder
	if preserved != "" {
		b.WriteString(preserved)
		if !strings.HasSuffix(preserved, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(block)

	next := b.String()
	if string(existing) == next {
		return false, nil
	}

	if err := os.WriteFile(path, []byte(next), 0o644); err != nil { //nolint:gosec // ignore file is non-secret
		return false, fmt.Errorf("write %s; %w", ignoreFileName, err)
	}

	return true, nil
}

// HasManagedIgnore reports whether the clone's .graphifyignore currently holds a
// krabby-managed block. Callers force a full graph rebuild in that case so the
// shrink guard cannot preserve stale nodes for now-excluded files.
func HasManagedIgnore(clonePath string) bool {
	b, err := os.ReadFile(filepath.Join(clonePath, ignoreFileName)) //nolint:gosec // clone root
	if err != nil {
		return false
	}

	return strings.Contains(string(b), managedBegin)
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
// pattern, mirroring graphify's gitignore semantics: a bare pattern matches at
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

// buildManagedBlock renders the managed section: the default patterns followed
// by the deduplicated extra patterns, wrapped in the begin/end markers.
func buildManagedBlock(extra []string) string {
	seen := make(map[string]bool, len(DefaultGraphIgnore)+len(extra))
	patterns := make([]string, 0, len(DefaultGraphIgnore)+len(extra))

	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		patterns = append(patterns, p)
	}

	for _, p := range DefaultGraphIgnore {
		add(p)
	}
	for _, p := range extra {
		add(p)
	}

	var b strings.Builder
	b.WriteString(managedBegin)
	b.WriteString("\n")
	for _, p := range patterns {
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString(managedEnd)
	b.WriteString("\n")

	return b.String()
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
