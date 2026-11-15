// Package graphbuilder adapts the embedded bag library for Krabby.
package graphbuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

const (
	// bagOutDir is where bag writes inside the tree it builds.
	bagOutDir       = ".bag"
	versionFileName = ".krabby-bag-version"
	// ignoreFileName holds the fingerprint of the exclude patterns a graph was
	// built with, so a later change to them is a string comparison rather than
	// a parse of the whole graph.
	ignoreFileName = ".krabby-bag-ignore"
)

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
	b, err := os.ReadFile(filepath.Join(filepath.Dir(GraphPath(repoPath)), versionFileName))

	return err == nil && strings.TrimSpace(string(b)) == c.version
}

// RecordGraphVersion marks a validated graph with the engine version that built it.
func (c *Builder) RecordGraphVersion(repoPath string) error {
	path := filepath.Join(filepath.Dir(GraphPath(repoPath)), versionFileName)
	if err := os.WriteFile(path, []byte(c.version+"\n"), 0o644); err != nil {
		return fmt.Errorf("record graph engine version; %w", err)
	}

	return nil
}

// Exclude returns the install-wide graph ignore patterns, for surfacing the
// effective configuration of one repository.
func (c *Builder) Exclude() []string { return slices.Clone(c.exclude) }

// GraphNeedsIgnoreRebuild reports whether the graph for repoPath was built
// with exclude rules other than the current ones, so the refresh path can
// rebuild it even when git did not change. extra carries the repository's own
// patterns, which count here too: changing one has to invalidate the existing
// graph, or it stays wrong until some unrelated commit triggers a rebuild.
//
// Any difference counts, in either direction. A removed pattern leaves files
// out of the graph that now belong in it, which no inspection of the graph
// could reveal. A graph with no recorded rules predates the record and is
// rebuilt once to gain it.
func (c *Builder) GraphNeedsIgnoreRebuild(repoPath string, extra []string) bool {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(GraphPath(repoPath)), ignoreFileName))

	return err != nil || strings.TrimSpace(string(b)) != ignoreFingerprint(c.buildExcludes(extra))
}

// buildExcludes is the exact pattern list handed to bag for one repository.
func (c *Builder) buildExcludes(extra []string) []string {
	return MergeExclude(DefaultGraphIgnore, c.ignorePatterns(extra))
}

// ignoreFingerprint identifies an ordered pattern list. Order is part of it on
// purpose: gitignore-style rules are evaluated in sequence, so the same
// patterns in another order are not guaranteed to select the same files.
func ignoreFingerprint(patterns []string) string {
	h := sha256.New()
	for _, p := range patterns {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
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
	// No configured timeout means no limit. WithTimeout(ctx, 0) would be
	// expired already, and every build would fail before it started.
	if c.buildTimeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, c.buildTimeout)
}

// clearOutputPath makes sure the graph's output directory inside a clone is a
// real directory of the clone, or absent.
//
// The clone is the repository's content, and a repository can commit .bag as
// a symlink. bag writes graph.json, the report and the HTML through that path,
// and krabby writes its markers beside them, so a link pointing elsewhere
// would have the build write outside the clone: into another repository's
// active snapshot, replacing its graph, or anywhere the process can write.
// A link (or any non-directory) at that name is removed; bag recreates the
// directory.
func clearOutputPath(repoPath string) error {
	path := filepath.Join(repoPath, bagOutDir)
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("inspect graph output dir; %w", err)
	case info.IsDir():
		return nil
	}

	slog.Warn("removing a committed non-directory where the graph output goes",
		"path", path, "mode", info.Mode().String())
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s from the clone; %w", bagOutDir, err)
	}

	return nil
}

// Update replaces the graph with a full AST-only build for repoPath.
// Code-only extraction needs no LLM key. extra carries the repository's own
// ignore patterns, unioned with the install-wide ones and passed directly to
// bag without writing files into the clone.
//
// Excluded files (testdata, fixtures, ...) can shrink the node count relative to
// an older graph, and bag's shrink guard would otherwise refuse to overwrite.
// Forcing is safe here because krabby only ever runs a
// deterministic full AST re-extraction (no partial LLM chunks to lose).
func (c *Builder) Update(ctx context.Context, repoPath string, extra []string) error {
	if err := clearOutputPath(repoPath); err != nil {
		return err
	}
	if _, err := RemoveLegacyManagedIgnore(repoPath); err != nil {
		// Non-fatal: duplicate exclusions in the legacy file do not change output.
		slog.Warn("bag: could not remove legacy managed ignore block", "path", repoPath, "error", err)
	}

	ctx, cancel := c.timeout(ctx)
	defer cancel()

	excludes := c.buildExcludes(extra)

	start := time.Now()
	result, err := bag.Build(ctx, repoPath, bag.BuildOptions{
		Excludes: excludes,
		Force:    true,
		// bag keys its AST cache by absolute file path and stores it inside
		// the tree it builds. Every build here runs in a freshly named staging
		// clone, so no entry could ever be read back: the cache would only cost
		// a write per source file and its size again in every kept snapshot.
		NoCache: true,
	})
	if err != nil {
		return fmt.Errorf("build graph; %w", err)
	}

	// Recorded here, by the call that applied the rules, so the record cannot
	// describe anything but the graph beside it.
	ignorePath := filepath.Join(result.OutDir, ignoreFileName)
	if err := os.WriteFile(ignorePath, []byte(ignoreFingerprint(excludes)+"\n"), 0o644); err != nil {
		return fmt.Errorf("record graph ignore rules; %w", err)
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

// LegacyGraphPath returns where bag releases before v0.2.0 wrote graph.json.
// A clone that only has this file needs a rebuild into the current layout.
func LegacyGraphPath(repoPath string) string {
	return filepath.Join(repoPath, "graphify-out", "graph.json")
}

// ReportPath returns the GRAPH_REPORT.md path for a scanned repository path.
func ReportPath(repoPath string) string {
	return bag.ReportPath(repoPath)
}

// HTMLPath returns the interactive graph.html path for a scanned repository path.
func HTMLPath(repoPath string) string {
	return bag.HTMLPath(repoPath)
}
