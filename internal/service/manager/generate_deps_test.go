package manager

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/registry"
)

func wantKeys(want map[string]bool) []string {
	keys := make([]string, 0, len(want))
	for k, v := range want {
		if v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	return keys
}

func TestResolveStageDeps(t *testing.T) {
	cases := []struct {
		name      string
		targets   []string
		graphOK   bool // graph output already present
		docsOK    bool // docs output already present
		wantAdded []string
	}{
		{
			name:      "docs_index pulls in docs and graph when both missing",
			targets:   []string{registry.StageDocsIndex},
			wantAdded: []string{registry.StageDocs, registry.StageDocsIndex, registry.StageGraph},
		},
		{
			name:      "docs_index reuses existing docs (docs already generated)",
			targets:   []string{registry.StageDocsIndex},
			docsOK:    true,
			graphOK:   true,
			wantAdded: []string{registry.StageDocsIndex},
		},
		{
			name:      "docs_index pulls docs but reuses existing graph",
			targets:   []string{registry.StageDocsIndex},
			graphOK:   true,
			wantAdded: []string{registry.StageDocs, registry.StageDocsIndex},
		},
		{
			name:      "docs pulls in graph when missing",
			targets:   []string{registry.StageDocs},
			wantAdded: []string{registry.StageDocs, registry.StageGraph},
		},
		{
			name:      "docs reuses existing graph",
			targets:   []string{registry.StageDocs},
			graphOK:   true,
			wantAdded: []string{registry.StageDocs},
		},
		{
			name:      "code_index pulls in graph when missing",
			targets:   []string{registry.StageCodeIndex},
			wantAdded: []string{registry.StageCodeIndex, registry.StageGraph},
		},
		{
			name:      "graph alone stays alone",
			targets:   []string{registry.StageGraph},
			wantAdded: []string{registry.StageGraph},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clone := t.TempDir()
			docsDir := t.TempDir()

			if tc.graphOK {
				mustWriteManagerTest(t, graphify.GraphPath(clone), "{}")
			}
			if tc.docsOK {
				mustWriteManagerTest(t, filepath.Join(docsDir, "documentation.md"), "# Docs")
			}

			repo := &registry.Repo{ID: "owner/repo", Path: clone}

			want := map[string]bool{}
			for _, s := range tc.targets {
				want[s] = true
			}

			mgr := &Manager{}
			mgr.resolveStageDeps(want, repo, docsDir, repo.ID)

			got := wantKeys(want)
			if len(got) != len(tc.wantAdded) {
				t.Fatalf("resolved stages = %v, want %v", got, tc.wantAdded)
			}
			for i := range got {
				if got[i] != tc.wantAdded[i] {
					t.Fatalf("resolved stages = %v, want %v", got, tc.wantAdded)
				}
			}
		})
	}
}

func TestDirHasMarkdown(t *testing.T) {
	empty := t.TempDir()
	if dirHasMarkdown(empty) {
		t.Fatalf("dirHasMarkdown(empty) = true, want false")
	}

	if dirHasMarkdown(filepath.Join(empty, "does-not-exist")) {
		t.Fatalf("dirHasMarkdown(missing) = true, want false")
	}

	withMd := t.TempDir()
	mustWriteManagerTest(t, filepath.Join(withMd, "documentation.md"), "# Docs")
	if !dirHasMarkdown(withMd) {
		t.Fatalf("dirHasMarkdown(with .md) = false, want true")
	}

	onlyJSON := t.TempDir()
	mustWriteManagerTest(t, filepath.Join(onlyJSON, "docs-index.json"), "{}")
	if dirHasMarkdown(onlyJSON) {
		t.Fatalf("dirHasMarkdown(no .md) = true, want false")
	}
}
