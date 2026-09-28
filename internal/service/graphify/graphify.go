// Package graphify provides the Graphify-compatible graph paths and build
// adapter used by Krabby. Graph construction runs in-process through bag.
package graphify

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rytsh/bag"
)

// Client builds Graphify-compatible graphs in-process.
type Client struct {
	version      string
	buildTimeout time.Duration
	exclude      []string
}

// TestedVersion is the linked bag extraction contract.
const TestedVersion = bag.EngineVersion

const versionFileName = ".krabby-bag-version"

// New creates an in-process graph builder. exclude carries extra gitignore-style
// patterns written into each clone's managed .graphifyignore block.
func New(buildTimeout time.Duration, exclude []string) *Client {
	return &Client{
		version:      bag.EngineVersion,
		buildTimeout: buildTimeout,
		exclude:      append([]string(nil), exclude...),
	}
}

// Version returns the linked bag extraction contract.
func (c *Client) Version() string { return c.version }

// GraphBuiltWithCurrentVersion reports whether Krabby recorded this engine version
// after validating the repository's active graph.
func (c *Client) GraphBuiltWithCurrentVersion(repoPath string) bool {
	b, err := os.ReadFile(filepath.Join(repoPath, "graphify-out", versionFileName))

	return err == nil && strings.TrimSpace(string(b)) == c.version
}

// RecordGraphVersion marks a validated graph with the CLI version that built it.
func (c *Client) RecordGraphVersion(repoPath string) error {
	path := filepath.Join(repoPath, "graphify-out", versionFileName)
	if err := os.WriteFile(path, []byte(c.version+"\n"), 0o644); err != nil {
		return fmt.Errorf("record graph engine version; %w", err)
	}

	return nil
}

// Exclude returns the install-wide graph ignore patterns, for surfacing the
// effective configuration of one repository.
func (c *Client) Exclude() []string { return c.exclude }

// GraphNeedsIgnoreRebuild reports whether the built graph for repoPath still
// contains nodes that the current exclude rules should drop, so the refresh path
// can rebuild a stale graph even when git did not change. extra carries the
// repository's own patterns, which must be considered here too: adding one to a
// repo has to invalidate its existing graph, or the excluded nodes survive
// until some unrelated commit happens to trigger a rebuild.
func (c *Client) GraphNeedsIgnoreRebuild(repoPath string, extra []string) bool {
	return GraphHasExcludedNodes(repoPath, c.ignorePatterns(extra))
}

// ignorePatterns is the effective exclude list for one repository: the
// install-wide patterns plus that repository's own. It is a union, never a
// replacement — an install-wide rule ("never graph vendored protobufs") is a
// policy, and a single repository opting out of it is not a case worth
// supporting.
func (c *Client) ignorePatterns(extra []string) []string {
	return MergeExclude(c.exclude, extra)
}

func (c *Client) timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, c.buildTimeout)
	return ctx, cancel
}

// Update runs an incremental (or initial) AST-only build for repoPath.
// Code-only extraction needs no LLM key. It first refreshes the clone's managed
// .graphifyignore block so the graph skips test fixtures and configured noise.
// extra carries the repository's own ignore patterns, unioned with the
// install-wide ones.
//
// The build is forced whenever a krabby-managed ignore block is present. Excluded
// files (testdata, fixtures, ...) shrink the node count relative to an older
// graph built without the ignore, and graphify's shrink guard would otherwise
// refuse to overwrite without --force — leaving stale excluded nodes in the
// graph forever. Forcing is safe here because krabby only ever runs a
// deterministic full AST re-extraction (no partial LLM chunks to lose).
func (c *Client) Update(ctx context.Context, repoPath string, extra []string) error {
	if _, err := WriteIgnore(repoPath, c.ignorePatterns(extra)); err != nil {
		// Non-fatal: a graph that includes testdata is still usable.
		slog.Warn("graphify: could not update .graphifyignore", "path", repoPath, "error", err)
	}

	ctx, cancel := c.timeout(ctx)
	defer cancel()

	start := time.Now()
	result, err := bag.Build(ctx, repoPath, bag.BuildOptions{
		Force: HasManagedIgnore(repoPath),
	})
	if err != nil {
		return fmt.Errorf("build graph; %w", err)
	}

	slog.Debug("bag graph build", "path", repoPath, "nodes", result.Nodes,
		"edges", result.Edges, "took", time.Since(start).String())
	return nil
}

// MergeGraphs merges graph files into out. Requires at least two inputs.
func (c *Client) MergeGraphs(ctx context.Context, out string, graphs ...string) error {
	ctx, cancel := c.timeout(ctx)
	defer cancel()

	start := time.Now()
	result, err := bag.MergeGraphs(ctx, out, graphs...)
	if err != nil {
		return fmt.Errorf("merge graphs; %w", err)
	}

	slog.Debug("bag graph merge", "path", out, "nodes", result.Nodes,
		"edges", result.Edges, "took", time.Since(start).String())
	return nil
}

// GraphPath returns the graph.json path for a scanned repository path.
func GraphPath(repoPath string) string {
	return filepath.Join(repoPath, "graphify-out", "graph.json")
}

// ReportPath returns the GRAPH_REPORT.md path for a scanned repository path.
func ReportPath(repoPath string) string {
	return filepath.Join(repoPath, "graphify-out", "GRAPH_REPORT.md")
}

// HTMLPath returns the interactive graph.html path for a scanned repository path.
func HTMLPath(repoPath string) string {
	return filepath.Join(repoPath, "graphify-out", "graph.html")
}
