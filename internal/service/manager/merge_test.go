package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMergeDisabledBehavior(t *testing.T) {
	dataDir := t.TempDir()
	mergedPath := filepath.Join(dataDir, "merged", "graph.json")

	// Pre-existing stale merged graph from a prior merge-enabled run.
	mustWriteManagerTest(t, mergedPath, `{"nodes":[],"links":[]}`)

	m := &Manager{mergedPath: mergedPath, mergeEnabled: false}

	// rebuildMerged is a no-op and never recreates the file.
	if err := m.rebuildMerged(context.Background()); err != nil {
		t.Fatalf("rebuildMerged: %v", err)
	}

	// Cleanup removes the stale file.
	m.CleanupMergedGraph()
	if _, err := os.Stat(mergedPath); !os.IsNotExist(err) {
		t.Fatalf("stale merged graph not removed: %v", err)
	}

	// MergedPath reports "unavailable" when disabled.
	if got := m.MergedPath(); got != "" {
		t.Errorf("MergedPath() = %q, want empty when merge disabled", got)
	}

	// Graph tools with no repo id are rejected with a helpful message.
	_, err := m.GraphPathFor(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("GraphPathFor(\"\") error = %v, want a 'disabled' message", err)
	}
}

func TestInferRepoID(t *testing.T) {
	repos := []string{"github.com/acme/auth-service", "github.com/acme/payments"}

	if got := inferRepoID(repos, map[string]any{"question": "How does auth service validate tokens?"}); got != repos[0] {
		t.Fatalf("inferRepoID() = %q, want %q", got, repos[0])
	}
	if got := inferRepoID(repos, map[string]any{"question": "How does request validation work?"}); got != "" {
		t.Fatalf("ambiguous inferRepoID() = %q, want empty", got)
	}
	if got := inferRepoID([]string{"a/foo-service", "b/foo_service"}, map[string]any{"question": "foo service"}); got != "" {
		t.Fatalf("duplicate-name inferRepoID() = %q, want empty", got)
	}
}

func TestGraphRepoSelectionResultIsBoundedAndActionable(t *testing.T) {
	var repos []string
	for i := range 25 {
		repos = append(repos, fmt.Sprintf("github.com/acme/repo-%02d", i))
	}

	result := graphRepoSelectionResult("query_graph", repos)
	text := result.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"Repository selection required", "Retry query_graph with repo", "list_repos", "search_code"} {
		if !strings.Contains(text, want) {
			t.Errorf("selection result missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "repo-24") {
		t.Fatalf("selection result exceeded bounded repo list: %s", text)
	}
}

func TestMergeEnabledKeepsFile(t *testing.T) {
	dataDir := t.TempDir()
	mergedPath := filepath.Join(dataDir, "merged", "graph.json")
	mustWriteManagerTest(t, mergedPath, `{"nodes":[],"links":[]}`)

	m := &Manager{mergedPath: mergedPath, mergeEnabled: true}

	// Cleanup must not touch the file when merging is enabled.
	m.CleanupMergedGraph()
	if _, err := os.Stat(mergedPath); err != nil {
		t.Fatalf("merged graph removed while enabled: %v", err)
	}

	if got := m.MergedPath(); got != mergedPath {
		t.Errorf("MergedPath() = %q, want %q", got, mergedPath)
	}
}
