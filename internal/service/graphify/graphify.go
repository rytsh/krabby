// Package graphify wraps the graphify CLI (build/update/merge) and python discovery.
package graphify

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Client shells out to the graphify CLI.
type Client struct {
	bin          string
	python       string
	buildTimeout time.Duration
	exclude      []string
}

// New creates a graphify CLI client. python may be empty; it is derived from
// the graphify binary shebang, falling back to python3. exclude carries extra
// gitignore-style patterns written into each clone's managed .graphifyignore
// block before a build so the graph skips test fixtures and other noise.
func New(bin, python string, buildTimeout time.Duration, exclude []string) (*Client, error) {
	binPath, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("graphify binary %q not found; install with `uv tool install graphifyy`; %w", bin, err)
	}

	if python == "" {
		python = pythonFromShebang(binPath)
	}

	return &Client{
		bin:          binPath,
		python:       python,
		buildTimeout: buildTimeout,
		exclude:      exclude,
	}, nil
}

// Python returns the interpreter able to `import graphify`.
func (c *Client) Python() string { return c.python }

// GraphNeedsIgnoreRebuild reports whether the built graph for repoPath still
// contains nodes that the current exclude rules should drop, so the refresh path
// can rebuild a stale graph even when git did not change.
func (c *Client) GraphNeedsIgnoreRebuild(repoPath string) bool {
	return GraphHasExcludedNodes(repoPath, c.exclude)
}

func pythonFromShebang(binPath string) string {
	f, err := os.Open(binPath)
	if err != nil {
		return "python3"
	}
	defer f.Close()

	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "python3"
	}

	line = strings.TrimSpace(strings.TrimPrefix(line, "#!"))
	if line == "" || strings.ContainsAny(line, " \t") || !filepath.IsAbs(line) {
		return "python3"
	}

	return line
}

func (c *Client) run(ctx context.Context, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, c.buildTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = dir

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	err := cmd.Run()
	slog.Debug("graphify run",
		"args", strings.Join(args, " "),
		"took", time.Since(start).String(),
		"output", truncate(out.String(), 2000),
	)

	if err != nil {
		return fmt.Errorf("graphify %s; %w; %s", strings.Join(args, " "), err, truncate(out.String(), 2000))
	}

	return nil
}

// Update runs an incremental (or initial) AST-only build for repoPath.
// Code-only extraction needs no LLM key. It first refreshes the clone's managed
// .graphifyignore block so the graph skips test fixtures and configured noise.
//
// The build is forced whenever a krabby-managed ignore block is present. Excluded
// files (testdata, fixtures, ...) shrink the node count relative to an older
// graph built without the ignore, and graphify's shrink guard would otherwise
// refuse to overwrite without --force — leaving stale excluded nodes in the
// graph forever. Forcing is safe here because krabby only ever runs a
// deterministic full AST re-extraction (no partial LLM chunks to lose).
func (c *Client) Update(ctx context.Context, repoPath string) error {
	if _, err := WriteIgnore(repoPath, c.exclude); err != nil {
		// Non-fatal: a graph that includes testdata is still usable.
		slog.Warn("graphify: could not update .graphifyignore", "path", repoPath, "error", err)
	}

	args := []string{"update", repoPath}
	if HasManagedIgnore(repoPath) {
		args = append(args, "--force")
	}

	return c.run(ctx, repoPath, args...)
}

// MergeGraphs merges graph files into out. Requires at least two inputs.
func (c *Client) MergeGraphs(ctx context.Context, out string, graphs ...string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("mkdir merged dir; %w", err)
	}

	args := append([]string{"merge-graphs"}, graphs...)
	args = append(args, "--out", out)

	return c.run(ctx, "", args...)
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}
