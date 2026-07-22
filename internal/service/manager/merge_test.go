package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
