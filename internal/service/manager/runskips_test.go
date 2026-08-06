package manager

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func TestNewRunSkipsNormalizesAndCascades(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "nothing requested",
			in:   nil,
		},
		{
			name: "docs drags its index along",
			in:   []string{registry.StageDocs},
			want: []string{registry.StageDocs, registry.StageDocsIndex},
		},
		{
			// The graph is a soft dependency: docs fall back to per-file
			// summaries and code chunking to line windows, so both still run.
			name: "graph cascades to nothing",
			in:   []string{registry.StageGraph},
			want: []string{registry.StageGraph},
		},
		{
			name: "case and whitespace are normalized",
			in:   []string{"  DOCS ", registry.StageDocs},
			want: []string{registry.StageDocs, registry.StageDocsIndex},
		},
		{
			name: "unknown names are dropped",
			in:   []string{"embeddings"},
		},
		{
			name: "everything",
			in: []string{
				registry.StageCodeIndex, registry.StageGraph,
				registry.StageDocsIndex, registry.StageDocs,
			},
			want: []string{
				registry.StageCodeIndex, registry.StageDocs,
				registry.StageDocsIndex, registry.StageGraph,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newRunSkips(tc.in).list()
			if !slices.Equal(got, tc.want) {
				t.Fatalf("newRunSkips(%v).list() = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// A run's skips are additive to the record's: a run may do less than the
// persisted overrides allow, never more.
func TestRunSkipsUnionWithOverrides(t *testing.T) {
	repo := &registry.Repo{
		ID:        "owner/repo",
		Overrides: registry.Overrides{SkipStages: []string{registry.StageGraph}},
	}
	skips := newRunSkips([]string{registry.StageDocs})

	for _, stage := range []string{registry.StageGraph, registry.StageDocs, registry.StageDocsIndex} {
		if !skips.skips(repo, stage) {
			t.Errorf("%s should be skipped", stage)
		}
	}
	if skips.skips(repo, registry.StageCodeIndex) {
		t.Error("code_index was asked for by neither the run nor the record")
	}

	// The zero value must behave exactly like the pre-existing repo-only check.
	var none runSkips
	if !none.skips(repo, registry.StageGraph) {
		t.Error("a nil runSkips must still honour the persisted override")
	}
	if none.skips(repo, registry.StageDocs) {
		t.Error("a nil runSkips must not invent skips")
	}
	if none.skips(nil, registry.StageGraph) {
		t.Error("a nil repo and no run skips must skip nothing")
	}
}

// The skip set is part of the queue dedup key so a partial refresh can never
// coalesce onto (and silently replace) a full one, and it is persisted in the
// spec so a restart replays the same reduced pipeline.
func TestRefreshTaskKeyAndSpecCarrySkip(t *testing.T) {
	m := &Manager{}

	full := m.refreshTask("owner/repo", nil)
	if full.Key != taskKindRefresh+":owner/repo" {
		t.Fatalf("full refresh key = %q", full.Key)
	}
	if full.Spec.Params != nil {
		t.Fatalf("full refresh must persist no skip params, got %v", full.Spec.Params)
	}

	partial := m.refreshTask("owner/repo", []string{registry.StageDocs})
	if partial.Key == full.Key {
		t.Fatal("a partial refresh must not share the full refresh dedup key")
	}
	// Cascade and ordering are baked into the key, so ["docs"] and
	// ["docs_index","docs"] describe the same work and do coalesce.
	if got := partial.Spec.Params["skip"]; got != "docs,docs_index" {
		t.Fatalf("persisted skip = %q, want %q", got, "docs,docs_index")
	}

	reordered := m.refreshTask("owner/repo", []string{registry.StageDocsIndex, registry.StageDocs})
	if reordered.Key != partial.Key {
		t.Fatalf("equivalent skip sets must share a key: %q vs %q", reordered.Key, partial.Key)
	}
}

// A restart must replay the reduced pipeline, not silently upgrade it to a full
// refresh that regenerates the documentation the caller asked to skip.
func TestRebuildTaskRestoresRefreshSkip(t *testing.T) {
	m := &Manager{}

	spec := m.refreshTask("owner/repo", []string{registry.StageDocs}).Spec
	restored, ok := m.rebuildTask(spec)
	if !ok {
		t.Fatal("refresh spec was not rebuildable")
	}

	want := m.refreshTask("owner/repo", []string{registry.StageDocs})
	if restored.Key != want.Key {
		t.Fatalf("restored key = %q, want %q", restored.Key, want.Key)
	}
	if restored.Spec.Params["skip"] != want.Spec.Params["skip"] {
		t.Fatalf("restored skip = %q, want %q",
			restored.Spec.Params["skip"], want.Spec.Params["skip"])
	}
}

// Skipping a stage for one run must not leave its recorded success behind: the
// docs-unchanged shortcut trusts StageOK, so a stale ok would strand the index
// on old markdown for every future refresh, not just the one that skipped it.
func TestInvalidateStageClearsRecordedSuccess(t *testing.T) {
	mgr, reg := newStageTestManager(t)
	ctx := context.Background()

	repo := &registry.Repo{ID: "owner/repo", URL: "https://git/owner/repo"}
	repo.Stages.DocsIndex.Status = registry.StageOK
	repo.Stages.DocsIndex.Commit = "abc123"
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	mgr.invalidateStage(ctx, repo, registry.StageDocsIndex)

	if repo.Stages.DocsIndex.Status != "" {
		t.Fatalf("stage status = %q, want cleared", repo.Stages.DocsIndex.Status)
	}

	stored, err := reg.Get(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Stages.DocsIndex.Status != "" || stored.Stages.DocsIndex.Commit != "" {
		t.Fatalf("stored stage = %+v, want cleared", stored.Stages.DocsIndex)
	}
}

// A failed or never-run stage carries no success to clear, and rewriting the
// record for it would drop the error a user still needs to see.
func TestInvalidateStageLeavesNonOKStates(t *testing.T) {
	mgr, reg := newStageTestManager(t)
	ctx := context.Background()

	repo := &registry.Repo{ID: "owner/repo2", URL: "https://git/owner/repo2"}
	repo.Stages.DocsIndex.Status = registry.StageError
	repo.Stages.DocsIndex.Error = "embedder unreachable"
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	mgr.invalidateStage(ctx, repo, registry.StageDocsIndex)

	if repo.Stages.DocsIndex.Error != "embedder unreachable" {
		t.Fatalf("stage error = %q, want preserved", repo.Stages.DocsIndex.Error)
	}
}

// TriggerRefresh with and without skips must produce two distinct queued tasks
// rather than one swallowing the other.
func TestTriggerRefreshDoesNotCoalesceAcrossSkipSets(t *testing.T) {
	ctx := context.Background()

	m := &Manager{queue: queue.New(ctx, 1)}
	t.Cleanup(m.queue.Close)

	release := blockQueue(t, m.queue) // occupy the slot so tasks stay queued
	t.Cleanup(release)

	m.TriggerRefresh("owner/repo")
	m.TriggerRefresh("owner/repo", registry.StageDocs)
	m.TriggerRefresh("owner/repo", registry.StageDocs) // identical: must coalesce

	queued := 0
	for _, it := range m.queue.Snapshot().Tasks {
		if it.Kind == taskKindRefresh && it.ID == "owner/repo" && it.State == queue.StateQueued {
			queued++
		}
	}
	if queued != 2 {
		t.Fatalf("queued refresh tasks = %d, want 2 (full + skip docs)", queued)
	}
}

// changedDocsGenerator reports that it rewrote the documentation and records
// how many times it ran, so a test can assert the docs stage was really skipped
// rather than merely producing nothing.
type changedDocsGenerator struct{ calls int }

func (g *changedDocsGenerator) Generate(
	context.Context, string, string, string, config.DocsOverride, bool,
) (*docgen.Manifest, error) {
	g.calls++

	return &docgen.Manifest{ChangedDocs: true}, nil
}

// newSkipTestManager wires the smallest manager that runs the docs and
// docs_index stages for real: a registry, a docs generator and a docs vector
// index backed by a fake embedder.
func newSkipTestManager(
	t *testing.T, gen docgen.Generator,
) (*Manager, *registry.Registry, vectorstore.Store, string) {
	t.Helper()

	emb, err := embedder.New(config.Embedder{
		BaseURL: reindexEmbedServer(t).URL,
		Model:   "fake",
		Batch:   10,
	})
	if err != nil {
		t.Fatal(err)
	}

	docsStore, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docsStore.Close() })

	docsRoot := t.TempDir()
	reg := newReindexRegistry(t)

	m := &Manager{
		reg:         reg,
		docsRootDir: docsRoot,
		activity:    map[string]map[string]struct{}{},
		progress:    map[string]map[string]Progress{},
		docs: &docsBundle{
			gen:   gen,
			rag:   rag.New(config.RAG{ChunkSize: 80, ChunkOverlap: 20}, emb, docsStore),
			store: docsStore,
		},
	}

	return m, reg, docsStore, docsRoot
}

// Skipping docs for one run must not call the generator, and must take the
// docs index down with it rather than re-embedding the previous run's markdown.
func TestBuildDocsAndIndexRunSkipDropsDocsAndItsIndex(t *testing.T) {
	ctx := context.Background()
	gen := &changedDocsGenerator{}
	m, reg, docsStore, docsRoot := newSkipTestManager(t, gen)

	repo := &registry.Repo{
		ID:         "owner/repo",
		URL:        "https://git/owner/repo",
		Path:       t.TempDir(),
		Status:     registry.StatusReady,
		LastCommit: "abc123",
	}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}
	mustWriteManagerTest(t, filepath.Join(docsRoot, "owner", "repo", "documentation.md"), "# Docs\n\nalpha")

	m.buildDocsAndIndex(ctx, repo, newRunSkips([]string{registry.StageDocs}), false, false)

	if gen.calls != 0 {
		t.Errorf("docs generator ran %d times, want 0", gen.calls)
	}

	paths, err := docsStore.IndexedPaths(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Errorf("docs index ran despite the docs cascade: %v", paths)
	}
	if repo.Stages.Docs.Status != "" || repo.Stages.DocsIndex.Status != "" {
		t.Errorf("skipped stages recorded state: docs=%q docs_index=%q",
			repo.Stages.Docs.Status, repo.Stages.DocsIndex.Status)
	}
}

// Skipping only the index while docs are regenerated leaves the index behind
// the markdown. The recorded success must be cleared so the next refresh's
// docs-unchanged shortcut cannot mistake the stale index for a current one.
func TestBuildDocsAndIndexRunSkipInvalidatesStaleDocsIndex(t *testing.T) {
	ctx := context.Background()
	gen := &changedDocsGenerator{}
	m, reg, docsStore, docsRoot := newSkipTestManager(t, gen)

	repo := &registry.Repo{
		ID:         "owner/repo",
		URL:        "https://git/owner/repo",
		Path:       t.TempDir(),
		Status:     registry.StatusReady,
		LastCommit: "abc123",
	}
	repo.Stages.DocsIndex = registry.StageState{Status: registry.StageOK, Commit: "old"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}
	mustWriteManagerTest(t, filepath.Join(docsRoot, "owner", "repo", "documentation.md"), "# Docs\n\nalpha")

	m.buildDocsAndIndex(ctx, repo, newRunSkips([]string{registry.StageDocsIndex}), false, false)

	if gen.calls != 1 {
		t.Errorf("docs generator ran %d times, want 1", gen.calls)
	}
	if paths, err := docsStore.IndexedPaths(ctx, repo.ID); err != nil {
		t.Fatal(err)
	} else if len(paths) != 0 {
		t.Errorf("docs_index ran despite being skipped: %v", paths)
	}
	if repo.Stages.DocsIndex.Status != "" {
		t.Fatalf("docs_index still reports %q; a later refresh would treat the stale index as current",
			repo.Stages.DocsIndex.Status)
	}
}
