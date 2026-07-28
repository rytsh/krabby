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
	version      string
	buildTimeout time.Duration
	exclude      []string
}

// TestedVersion is the Graphify release exercised by Krabby's integration test
// and installed in the published container image.
const TestedVersion = "0.9.26"

const versionFileName = ".krabby-graphify-version"

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
		version:      detectVersion(binPath),
		buildTimeout: buildTimeout,
		exclude:      exclude,
	}, nil
}

// Python returns the interpreter able to `import graphify`.
func (c *Client) Python() string { return c.python }

// Version returns the installed Graphify version, or "unknown" when the CLI
// does not support version discovery.
func (c *Client) Version() string { return c.version }

// GraphBuiltWithCurrentVersion reports whether Krabby recorded this CLI version
// after validating the repository's active graph.
func (c *Client) GraphBuiltWithCurrentVersion(repoPath string) bool {
	b, err := os.ReadFile(filepath.Join(repoPath, "graphify-out", versionFileName))

	return err == nil && strings.TrimSpace(string(b)) == c.version
}

// RecordGraphVersion marks a validated graph with the CLI version that built it.
func (c *Client) RecordGraphVersion(repoPath string) error {
	path := filepath.Join(repoPath, "graphify-out", versionFileName)
	if err := os.WriteFile(path, []byte(c.version+"\n"), 0o644); err != nil {
		return fmt.Errorf("record graphify version; %w", err)
	}

	return nil
}

func detectVersion(binPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, binPath, "--version").CombinedOutput()
	if err != nil {
		return "unknown"
	}

	fields := strings.Fields(string(out))
	if len(fields) >= 2 && strings.EqualFold(fields[0], "graphify") {
		return fields[1]
	}
	if len(fields) == 0 {
		return "unknown"
	}

	return truncate(strings.Join(fields, " "), 128)
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
