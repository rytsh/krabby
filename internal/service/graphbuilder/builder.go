// Package graphbuilder adapts the embedded bag library for Krabby.
package graphbuilder

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

// Builder builds Graphify-compatible graphs in-process.
type Builder struct {
	version      string
	buildTimeout time.Duration
	exclude      []string
}

// TestedVersion is the linked bag extraction contract.
const TestedVersion = bag.EngineVersion

const versionFileName = ".krabby-bag-version"

// New creates an in-process graph builder. exclude carries extra gitignore-style
// patterns passed directly to bag for every build.
func New(buildTimeout time.Duration, exclude []string) *Builder {
	return &Builder{
		version:      bag.EngineVersion,
		buildTimeout: buildTimeout,
		exclude:      append([]string(nil), exclude...),
	}
}

// Version returns the linked bag extraction contract.
func (c *Builder) Version() string { return c.version }

// GraphBuiltWithCurrentVersion reports whether Krabby recorded this engine version
// after validating the repository's active graph.
func (c *Builder) GraphBuiltWithCurrentVersion(repoPath string) bool {
	b, err := os.ReadFile(filepath.Join(repoPath, "graphify-out", versionFileName))

	return err == nil && strings.TrimSpace(string(b)) == c.version
}

// RecordGraphVersion marks a validated graph with the CLI version that built it.
func (c *Builder) RecordGraphVersion(repoPath string) error {
	path := filepath.Join(repoPath, "graphify-out", versionFileName)
	if err := os.WriteFile(path, []byte(c.version+"\n"), 0o644); err != nil {
		return fmt.Errorf("record graph engine version; %w", err)
	}

	return nil
}

// Exclude returns the install-wide graph ignore patterns, for surfacing the
// effective configuration of one repository.
func (c *Builder) Exclude() []string { return c.exclude }

// GraphNeedsIgnoreRebuild reports whether the built graph for repoPath still
// contains nodes that the current exclude rules should drop, so the refresh path
// can rebuild a stale graph even when git did not change. extra carries the
// repository's own patterns, which must be considered here too: adding one to a
// repo has to invalidate its existing graph, or the excluded nodes survive
// until some unrelated commit happens to trigger a rebuild.
func (c *Builder) GraphNeedsIgnoreRebuild(repoPath string, extra []string) bool {
	return GraphHasExcludedNodes(repoPath, c.ignorePatterns(extra))
}

// ignorePatterns is the effective exclude list for one repository: the
// install-wide patterns plus that repository's own. It is a union, never a
// replacement — an install-wide rule ("never graph vendored protobufs") is a
// policy, and a single repository opting out of it is not a case worth
// supporting.
func (c *Builder) ignorePatterns(extra []string) []string {
	return MergeExclude(c.exclude, extra)
}

func (c *Builder) timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, c.buildTimeout)
	return ctx, cancel
}

// Update runs an incremental (or initial) AST-only build for repoPath.
// Code-only extraction needs no LLM key. extra carries the repository's own
// ignore patterns, unioned with the install-wide ones and passed directly to
// bag without writing files into the clone.
//
// Excluded files (testdata, fixtures, ...) can shrink the node count relative to
// an older graph, and bag's shrink guard would otherwise refuse to overwrite.
// Forcing is safe here because krabby only ever runs a
// deterministic full AST re-extraction (no partial LLM chunks to lose).
func (c *Builder) Update(ctx context.Context, repoPath string, extra []string) error {
	if _, err := RemoveLegacyManagedIgnore(repoPath); err != nil {
		// Non-fatal: duplicate exclusions in the legacy file do not change output.
		slog.Warn("bag: could not remove legacy managed ignore block", "path", repoPath, "error", err)
	}

	ctx, cancel := c.timeout(ctx)
	defer cancel()

	start := time.Now()
	result, err := bag.Build(ctx, repoPath, bag.BuildOptions{
		Excludes: MergeExclude(DefaultGraphIgnore, c.ignorePatterns(extra)),
		Force:    true,
	})
	if err != nil {
		return fmt.Errorf("build graph; %w", err)
	}

	slog.Debug("bag graph build", "path", repoPath, "nodes", result.Nodes,
		"edges", result.Edges, "took", time.Since(start).String())
	return nil
}

// MergeGraphs merges graph files into out. Requires at least two inputs.
func (c *Builder) MergeGraphs(ctx context.Context, out string, graphs ...string) error {
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
	return bag.GraphPath(repoPath)
}

// ReportPath returns the GRAPH_REPORT.md path for a scanned repository path.
func ReportPath(repoPath string) string {
	return bag.ReportPath(repoPath)
}

// HTMLPath returns the interactive graph.html path for a scanned repository path.
func HTMLPath(repoPath string) string {
	return bag.HTMLPath(repoPath)
}
