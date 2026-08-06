package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

func TestSkipsStage(t *testing.T) {
	repo := &registry.Repo{
		ID:        "owner/deploy",
		Overrides: registry.Overrides{SkipStages: []string{registry.StageGraph}},
	}

	if !skipsStage(repo, registry.StageGraph) {
		t.Error("graph should be skipped")
	}
	if skipsStage(repo, registry.StageDocs) {
		t.Error("docs was not skipped")
	}

	// A record we could not load must not silently disable work.
	if skipsStage(nil, registry.StageGraph) {
		t.Error("a nil repo must skip nothing")
	}
}

// A skipped stage is never pulled back in as somebody else's prerequisite:
// both dependents of the graph degrade gracefully without it, so the dependent
// still runs while the opt-out is honoured.
func TestResolveStageDepsHonoursSkip(t *testing.T) {
	cases := []struct {
		name    string
		targets []string
		skip    []string
		want    []string
	}{
		{
			name:    "docs does not pull in a skipped graph",
			targets: []string{registry.StageDocs},
			skip:    []string{registry.StageGraph},
			want:    []string{registry.StageDocs},
		},
		{
			name:    "code_index does not pull in a skipped graph",
			targets: []string{registry.StageCodeIndex},
			skip:    []string{registry.StageGraph},
			want:    []string{registry.StageCodeIndex},
		},
		{
			name:    "docs_index still pulls docs when only graph is skipped",
			targets: []string{registry.StageDocsIndex},
			skip:    []string{registry.StageGraph},
			want:    []string{registry.StageDocs, registry.StageDocsIndex},
		},
		{
			name:    "an unskipped graph is still pulled in",
			targets: []string{registry.StageDocs},
			want:    []string{registry.StageDocs, registry.StageGraph},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &registry.Repo{
				ID:        "owner/repo",
				Path:      t.TempDir(),
				Overrides: registry.Overrides{SkipStages: tc.skip},
			}

			want := map[string]bool{}
			for _, s := range tc.targets {
				want[s] = true
			}

			mgr := &Manager{}
			mgr.resolveStageDeps(want, repo, t.TempDir(), repo.ID)

			got := wantKeys(want)
			if len(got) != len(tc.want) {
				t.Fatalf("resolved stages = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("resolved stages = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Asking for a stage the repo opted out of is an error naming the override,
// not a no-op that reports success and produces nothing.
func TestGenerateRejectsSkippedTarget(t *testing.T) {
	mgr, reg := newStageTestManager(t)
	ctx := context.Background()

	repo := &registry.Repo{
		ID:        "owner/deploy",
		URL:       "https://git/owner/deploy",
		Path:      cloneWithGit(t),
		Overrides: registry.Overrides{SkipStages: []string{registry.StageGraph, registry.StageCodeIndex}},
	}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	err := mgr.Generate(ctx, repo.ID, []string{registry.StageGraph}, false)
	if err == nil {
		t.Fatal("expected an error for a skipped target")
	}
	if !strings.Contains(err.Error(), "skip_stages") {
		t.Errorf("error should name the override, got %v", err)
	}
	if !strings.Contains(err.Error(), registry.StageGraph) {
		t.Errorf("error should name the blocked stage, got %v", err)
	}
}

func TestGenerateRejectsSkippedTargetAmongAllowedOnes(t *testing.T) {
	mgr, reg := newStageTestManager(t)
	ctx := context.Background()

	repo := &registry.Repo{
		ID:        "owner/deploy2",
		URL:       "https://git/owner/deploy2",
		Path:      cloneWithGit(t),
		Overrides: registry.Overrides{SkipStages: []string{registry.StageCodeIndex}},
	}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	err := mgr.Generate(ctx, repo.ID, []string{registry.StageDocs, registry.StageCodeIndex}, false)
	if err == nil || !strings.Contains(err.Error(), registry.StageCodeIndex) {
		t.Fatalf("a partially blocked request must fail naming the blocked stage, got %v", err)
	}
}

// newStageTestManager builds a Manager with just enough wiring for Generate's
// pre-flight checks: a registry and the lazily initialized lock map.
func newStageTestManager(t *testing.T) (*Manager, *registry.Registry) {
	t.Helper()

	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	return &Manager{reg: reg, reposDir: t.TempDir()}, reg
}

// cloneWithGit returns a directory that passes Generate's "has a clone" check.
func cloneWithGit(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	return dir
}
