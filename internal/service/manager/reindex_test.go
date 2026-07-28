package manager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/storage"
)

type unchangedDocsGenerator struct{}

func (unchangedDocsGenerator) Generate(
	context.Context, string, string, string, config.DocsOverride, bool,
) (*docgen.Manifest, error) {
	return &docgen.Manifest{ChangedDocs: false}, nil
}

func reindexEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		resp := struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}{Data: make([]struct {
			Embedding []float32 `json:"embedding"`
		}, len(req.Input))}
		for i := range resp.Data {
			resp.Data[i].Embedding = []float32{1, float32(i + 1), 0.5}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func newReindexRegistry(t *testing.T) *registry.Registry {
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

	return reg
}

// blockQueue fills the queue's single slot with a task that blocks until the
// returned release func is called, so subsequently submitted tasks stay in the
// queued state and can be observed via Snapshot without racing execution.
func blockQueue(t *testing.T, q *queue.Queue) func() {
	t.Helper()

	release := make(chan struct{})
	started := make(chan struct{})
	q.Submit(queue.Task{
		ID:   "blocker",
		Kind: "blocker",
		Key:  "blocker",
		Run: func(ctx context.Context) error {
			close(started)
			<-release
			return nil
		},
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocker task did not start")
	}

	return func() { close(release) }
}

// waitForReindex polls the queue snapshot until a reindex task for id appears
// (in any state), or the deadline elapses.
func waitForReindex(t *testing.T, q *queue.Queue, id string) queue.Item {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, it := range q.Snapshot().Tasks {
			if it.Kind == taskKindReindex && it.ID == id {
				return it
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("no reindex task queued for %q", id)

	return queue.Item{}
}

// TestBuildDocsAndIndexEmptyBundleDefersReindex verifies that when the live
// docs/code bundle is unexpectedly empty (the transient state of a Close or a
// Configure swap), buildDocsAndIndex does not silently drop the work: it
// re-queues a reindex so a healthy bundle can pick it up later. This guards the
// regression where a settings change that coincided with a repo's post-graph
// index phase left docs/code_index permanently empty with no error.
func TestBuildDocsAndIndexEmptyBundleDefersReindex(t *testing.T) {
	ctx := context.Background()

	m := &Manager{
		// An empty bundle: every capability nil, exactly what Close installs
		// and what a mid-swap Configure can momentarily expose.
		docs:     &docsBundle{},
		queue:    queue.New(ctx, 1),
		activity: map[string]map[string]struct{}{},
	}
	t.Cleanup(m.queue.Close)

	release := blockQueue(t, m.queue) // occupy the slot so the reindex stays queued
	t.Cleanup(release)

	repo := &registry.Repo{ID: "owner/repo", Status: registry.StatusReady}

	m.buildDocsAndIndex(ctx, repo, true, false)

	it := waitForReindex(t, m.queue, repo.ID)
	if it.State != queue.StateQueued {
		t.Fatalf("reindex task state = %q, want %q", it.State, queue.StateQueued)
	}
}

// TestBuildDocsAndIndexEmptyBundleNoRequeueOnReindexPath verifies the reindex
// path (deferReindex=false) does not re-queue itself on an empty bundle, which
// would busy-loop the queue.
func TestBuildDocsAndIndexEmptyBundleNoRequeueOnReindexPath(t *testing.T) {
	ctx := context.Background()

	m := &Manager{
		docs:     &docsBundle{},
		queue:    queue.New(ctx, 1),
		activity: map[string]map[string]struct{}{},
	}
	t.Cleanup(m.queue.Close)

	release := blockQueue(t, m.queue)
	t.Cleanup(release)

	repo := &registry.Repo{ID: "owner/repo", Status: registry.StatusReady}

	m.buildDocsAndIndex(ctx, repo, false, true)

	// Give any (erroneous) submit a moment to land, then assert none did.
	time.Sleep(50 * time.Millisecond)
	for _, it := range m.queue.Snapshot().Tasks {
		if it.Kind == taskKindReindex && it.ID == repo.ID {
			t.Fatal("reindex path re-queued itself on empty bundle (busy-loop risk)")
		}
	}
}

// TestScheduleReindexDedups verifies the queue key collapses repeated reindex
// requests for the same repo into a single queued task.
func TestScheduleReindexDedups(t *testing.T) {
	ctx := context.Background()

	m := &Manager{
		queue: queue.New(ctx, 1),
	}
	t.Cleanup(m.queue.Close)

	release := blockQueue(t, m.queue) // occupy the slot so tasks stay queued
	t.Cleanup(release)

	m.scheduleReindex("owner/repo")
	m.scheduleReindex("owner/repo")
	m.scheduleReindex("owner/repo")

	count := 0
	for _, it := range m.queue.Snapshot().Tasks {
		if it.Kind == taskKindReindex && it.ID == "owner/repo" {
			count++
		}
	}

	if count != 1 {
		t.Fatalf("queued reindex tasks = %d, want 1 (deduped)", count)
	}
}

// TestSettingsReindexFullyRebuildsSemanticIndexes pins the settings reindex
// contract: an unchanged commit and an existing docs vector must not suppress
// backfill after RAG is enabled again or its model/chunking settings change.
func TestSettingsReindexFullyRebuildsSemanticIndexes(t *testing.T) {
	ctx := context.Background()
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
	codeStore, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = codeStore.Close() })

	docsRAG := rag.New(config.RAG{ChunkSize: 80, ChunkOverlap: 20}, emb, docsStore)
	codeRAG := coderag.New(config.CodeRAG{ChunkSize: 80, ChunkOverlap: 20}, emb, codeStore, nil, nil)
	docsRoot := t.TempDir()
	clone := t.TempDir()
	repo := &registry.Repo{
		ID:         "owner/repo",
		Path:       clone,
		Status:     registry.StatusReady,
		LastCommit: "abc123",
	}
	repo.Stages.DocsIndex = registry.StageState{Status: registry.StageOK, Commit: repo.LastCommit}
	repo.Stages.CodeIndex = registry.StageState{Status: registry.StageOK, Commit: repo.LastCommit}

	reg := newReindexRegistry(t)
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	docsDir := filepath.Join(docsRoot, "owner", "repo")
	mustWriteManagerTest(t, filepath.Join(docsDir, "documentation.md"), "# Existing\n\nalpha")
	if err := docsRAG.Index(ctx, repo.ID, docsDir); err != nil {
		t.Fatalf("seed docs vectors: %v", err)
	}
	// This file represents documentation added while semantic RAG was disabled.
	mustWriteManagerTest(t, filepath.Join(docsDir, "new.md"), "# New\n\nbeta")
	mustWriteManagerTest(t, filepath.Join(clone, "main.go"), "package main\n\nfunc main() {}\n")

	m := &Manager{
		reg:         reg,
		docsRootDir: docsRoot,
		activity:    map[string]map[string]struct{}{},
		progress:    map[string]map[string]Progress{},
		docs: &docsBundle{
			gen:       unchangedDocsGenerator{},
			rag:       docsRAG,
			store:     docsStore,
			codeRag:   codeRAG,
			codeStore: codeStore,
		},
	}

	m.buildDocsAndIndex(ctx, repo, false, true)

	docPaths, err := docsStore.IndexedPaths(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := docPaths["new.md"]; !ok {
		t.Fatalf("settings reindex did not backfill new docs vectors: %v", docPaths)
	}
	codePaths, err := codeStore.IndexedPaths(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := codePaths["main.go"]; !ok {
		t.Fatalf("settings reindex did not backfill code vectors at unchanged commit: %v", codePaths)
	}
}

// TestConfigureDisabledKeepsVectorsAsInactiveCache documents disable semantics:
// services are detached immediately, while derived vectors stay on disk and are
// replaced by the full settings reindex if the feature is enabled again.
func TestConfigureDisabledKeepsVectorsAsInactiveCache(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	codeDir := t.TempDir()
	docsStore, err := vectorstore.New(docsDir)
	if err != nil {
		t.Fatal(err)
	}
	codeStore, err := vectorstore.New(codeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []vectorstore.Store{docsStore, codeStore} {
		if err := store.Upsert(ctx, []vectorstore.Item{{
			ID: "cached", Vector: []float32{1, 0, 0},
			Payload: vectorstore.Payload{Repo: "owner/repo", DocPath: "cached.md", Chunk: "cached"},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	m := &Manager{
		docsVectorsDir: docsDir,
		codeVectorsDir: codeDir,
		docs: &docsBundle{
			rag:       &rag.Service{},
			store:     docsStore,
			codeRag:   &coderag.Service{},
			codeStore: codeStore,
		},
	}
	if err := m.Configure(ctx, settings.Settings{}); err != nil {
		t.Fatal(err)
	}
	if m.docs.rag != nil || m.docs.codeStore != nil {
		t.Fatal("disabled settings left a semantic service active")
	}
	if _, err := m.SearchCode(ctx, "owner/repo", "", "query", 1); !errors.Is(err, ErrCodeRAGDisabled) {
		t.Fatalf("SearchCode error = %v, want ErrCodeRAGDisabled", err)
	}
	if _, err := m.retrieveSemanticCandidates(ctx, vectorstore.FilterKey("owner/repo"), "query", 1); err == nil {
		t.Fatal("semantic docs search remained active after RAG was disabled")
	}

	reopenedDocs, err := vectorstore.New(docsDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedDocs.Close() })
	reopenedCode, err := vectorstore.New(codeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedCode.Close() })
	for name, store := range map[string]vectorstore.Store{"docs": reopenedDocs, "code": reopenedCode} {
		has, err := store.HasRepo(ctx, "owner/repo")
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("disabling %s RAG unexpectedly purged its vector cache", name)
		}
	}
}
