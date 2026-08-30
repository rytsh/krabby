package manager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
)

// fakeReconcileFetcher mimics an incremental provider (Confluence/JIRA): the
// first fetch (nil state) returns the full page set and an advanced watermark;
// every later fetch (non-nil state) reports "nothing changed since the
// watermark" by returning no pages. This is the shape that broke the sync-time
// reconcile, which keyed off only the pages fetched this run.
type fakeReconcileFetcher struct {
	pages []websource.RemotePage
}

func (f *fakeReconcileFetcher) Validate(json.RawMessage) error { return nil }

func (f *fakeReconcileFetcher) MergeConfig(_, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 {
		return json.RawMessage(`{}`), nil
	}

	return update, nil
}

func (f *fakeReconcileFetcher) ConfigView(json.RawMessage) any { return struct{}{} }

func (f *fakeReconcileFetcher) Fetch(_ context.Context, _ *websource.Collection, _ []*websource.Page, state json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	if len(state) != 0 {
		// Incremental run: nothing changed since the stored watermark.
		return &websource.FetchResult{State: state}, nil
	}

	// First, full discovery run.
	for _, p := range f.pages {
		if err := emit(p); err != nil {
			return nil, err
		}
	}

	return &websource.FetchResult{
		Complete: true,
		State:    json.RawMessage(`{"w":"1"}`),
	}, nil
}

// fakeReconcileEmbedServer is an OpenAI-compatible /embeddings endpoint that
// returns a deterministic 3-dim vector counting the words alpha/beta/gamma, so
// tests can index and retrieve without a real embedding provider.
func fakeReconcileEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		type datum struct {
			Embedding []float32 `json:"embedding"`
		}
		var resp struct {
			Data []datum `json:"data"`
		}
		for _, text := range req.Input {
			lower := strings.ToLower(text)
			resp.Data = append(resp.Data, datum{Embedding: []float32{
				float32(strings.Count(lower, "alpha")) + 0.01,
				float32(strings.Count(lower, "beta")) + 0.01,
				float32(strings.Count(lower, "gamma")) + 0.01,
			}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// newReconcileManager builds a minimal Manager wired with a live RAG service
// (fake embedder + real embedded vector store), an in-memory web-source store
// and a single "fake" fetcher, enough to exercise RefreshWebSource end to end.
func newReconcileManager(t *testing.T, fetcher websource.Fetcher) (*Manager, *websource.Store) {
	return newReconcileManagerWithDeps(t, fetcher, "", nil)
}

func newReconcileManagerWithDeps(
	t *testing.T,
	fetcher websource.Fetcher,
	embedURL string,
	wrapStore func(vectorstore.Store) vectorstore.Store,
) (*Manager, *websource.Store) {
	t.Helper()
	ctx := context.Background()

	customEmbedder := embedURL != ""
	if embedURL == "" {
		embedURL = fakeReconcileEmbedServer(t).URL
	}
	embedCfg := config.Embedder{BaseURL: embedURL, Model: "fake"}
	if customEmbedder {
		embedCfg.Batch = 100
		embedCfg.Concurrency = 1
	}
	emb, err := embedder.New(embedCfg)
	if err != nil {
		t.Fatalf("embedder.New: %v", err)
	}

	store, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("vectorstore.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if wrapStore != nil {
		store = wrapStore(store)
	}

	ragSvc := rag.New(config.RAG{ChunkSize: 200, ChunkOverlap: 40, TopK: 20, TopDocs: 5}, emb, store)

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatalf("bw.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	webStore, err := websource.New(db)
	if err != nil {
		t.Fatalf("websource.New: %v", err)
	}
	docsText, err := rag.NewTextStore(db)
	if err != nil {
		t.Fatalf("rag.NewTextStore: %v", err)
	}

	m := &Manager{
		queue:          queue.New(ctx, 1),
		locks:          map[string]*sync.Mutex{},
		activity:       map[string]map[string]struct{}{},
		progress:       map[string]map[string]Progress{},
		sourcesRootDir: t.TempDir(),
		webStore:       webStore,
		webFetchers:    map[string]websource.Fetcher{"fake": fetcher},
		docsText:       docsText,
		docs:           &docsBundle{rag: ragSvc, store: store},
	}
	t.Cleanup(m.queue.Close)

	return m, webStore
}

type durableWebRun struct {
	pages    []websource.RemotePage
	complete bool
	state    json.RawMessage
}

type durableWebFetcher struct {
	runs []durableWebRun
	call int
}

func (f *durableWebFetcher) Validate(json.RawMessage) error { return nil }

func (f *durableWebFetcher) MergeConfig(_, update json.RawMessage) (json.RawMessage, error) {
	return update, nil
}

func (f *durableWebFetcher) ConfigView(json.RawMessage) any { return struct{}{} }

func (f *durableWebFetcher) Fetch(_ context.Context, _ *websource.Collection, _ []*websource.Page, _ json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	run := f.runs[f.call]
	f.call++
	for _, page := range run.pages {
		if err := emit(page); err != nil {
			return nil, err
		}
	}

	return &websource.FetchResult{Complete: run.complete, State: run.state}, nil
}

type faultEmbeddingServer struct {
	server *httptest.Server
	armed  atomic.Bool
	calls  atomic.Int64
	failed atomic.Bool
}

func newFaultEmbeddingServer(t *testing.T) *faultEmbeddingServer {
	t.Helper()
	f := &faultEmbeddingServer{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 512-chunk RAG flush takes six deterministic HTTP batches. Failing
		// the seventh request leaves that first flush durably upserted.
		if f.armed.Load() && f.calls.Add(1) == 7 && f.failed.CompareAndSwap(false, true) {
			http.Error(w, "injected embedding failure", http.StatusBadRequest)

			return
		}

		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type datum struct {
			Embedding []float32 `json:"embedding"`
		}
		resp := struct {
			Data []datum `json:"data"`
		}{Data: make([]datum, 0, len(req.Input))}
		for _, text := range req.Input {
			lower := strings.ToLower(text)
			resp.Data = append(resp.Data, datum{Embedding: []float32{
				float32(strings.Count(lower, "alpha")) + 0.01,
				float32(strings.Count(lower, "beta")) + 0.01,
				float32(strings.Count(lower, "gamma")) + 0.01,
			}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.server.Close)

	return f
}

func (f *faultEmbeddingServer) arm() {
	f.calls.Store(0)
	f.armed.Store(true)
}

type faultDeleteStore struct {
	vectorstore.Store
	armed  atomic.Bool
	failed atomic.Bool
}

func (s *faultDeleteStore) DeletePaths(ctx context.Context, repo string, paths []string) error {
	if s.armed.Load() && len(paths) > 1 && s.failed.CompareAndSwap(false, true) {
		if err := s.Store.DeletePaths(ctx, repo, paths[:1]); err != nil {
			return err
		}

		return errors.New("injected partial delete failure")
	}

	return s.Store.DeletePaths(ctx, repo, paths)
}

// TestRefreshWebSourceReembedsMissingOnIncrementalSync is the regression test
// for the sync-time reconcile: pressing "Sync" on an incremental source
// (Confluence/JIRA) must re-embed pages whose markdown exists on disk but whose
// vectors are missing — e.g. after a vector-store migration dropped all rows —
// even though the incremental fetch returns no pages that run. Previously the
// reconcile keyed off only the pages fetched this run, so a routine incremental
// sync (which returns nothing) never repaired the index, leaving the source
// unsearchable and its recency dates unprocessed.
func TestRefreshWebSourceReembedsMissingOnIncrementalSync(t *testing.T) {
	ctx := context.Background()
	updated := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

	fetcher := &fakeReconcileFetcher{pages: []websource.RemotePage{
		{Slug: "alpha", Title: "Alpha", URL: "https://x/alpha", Markdown: "# Alpha\n\nalpha alpha alpha\n", UpdatedAt: updated},
		{Slug: "beta", Title: "Beta", URL: "https://x/beta", Markdown: "# Beta\n\nbeta beta beta\n", UpdatedAt: updated},
	}}

	m, webStore := newReconcileManager(t, fetcher)

	col := &websource.Collection{
		Name: "wiki", Type: "fake", Status: websource.StatusPending, Config: json.RawMessage(`{}`),
	}
	if err := webStore.UpsertCollection(ctx, col); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	// First sync: a full fetch that writes and embeds both pages.
	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	scope := websource.ScopeKey("wiki")
	indexed, err := m.docs.rag.IndexedPaths(ctx, scope)
	if err != nil {
		t.Fatalf("IndexedPaths after first sync: %v", err)
	}
	if len(indexed) != 2 {
		t.Fatalf("after first sync indexed = %v, want 2 docs", indexed)
	}
	textIndexed, err := m.docsText.IndexedPaths(ctx, scope)
	if err != nil || len(textIndexed) != 2 {
		t.Fatalf("after first sync text indexed = %v, error = %v", textIndexed, err)
	}

	// Simulate the vector-store migration that drops all rows (the bucketVersion
	// bump): the markdown files and page records survive, but the vectors are
	// gone and the collection now carries an advanced incremental watermark.
	if err := m.docs.rag.DeleteRepo(ctx, scope); err != nil {
		t.Fatalf("drop vectors: %v", err)
	}
	if err := m.docsText.DeleteRepo(ctx, scope); err != nil {
		t.Fatalf("drop docs text: %v", err)
	}
	if idx, _ := m.docs.rag.IndexedPaths(ctx, scope); len(idx) != 0 {
		t.Fatalf("precondition: expected vectors dropped, got %v", idx)
	}

	// Second sync is incremental and returns no pages ("nothing changed"). The
	// reconcile must still re-embed the pages whose markdown is on disk but
	// whose vectors are missing.
	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	indexed, err = m.docs.rag.IndexedPaths(ctx, scope)
	if err != nil {
		t.Fatalf("IndexedPaths after second sync: %v", err)
	}
	if _, ok := indexed["alpha.md"]; !ok {
		t.Fatalf("alpha.md not re-embedded on incremental sync: %v", indexed)
	}
	if _, ok := indexed["beta.md"]; !ok {
		t.Fatalf("beta.md not re-embedded on incremental sync: %v", indexed)
	}
	textIndexed, err = m.docsText.IndexedPaths(ctx, scope)
	if err != nil || len(textIndexed) != 2 {
		t.Fatalf("text index not repaired: %v, error = %v", textIndexed, err)
	}

	// The source's recency date is carried onto the re-embedded vectors, taken
	// from the persisted page record (the reconcile does not re-fetch).
	docs, err := m.docs.rag.Retrieve(ctx, vectorstore.FilterKey(scope), "alpha", 1)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Retrieve returned %d docs, want 1", len(docs))
	}
	if !docs[0].UpdatedAt.Equal(updated) {
		t.Fatalf("re-embedded doc UpdatedAt = %v, want %v", docs[0].UpdatedAt, updated)
	}
}

func TestRefreshWebSourceRetriesDirtyPathAfterPartialVectorInsert(t *testing.T) {
	ctx := context.Background()
	initial := remotePage("alpha")
	changed := initial
	changed.Markdown = "# Alpha\n\n" + strings.Repeat("alpha ", 15000) + "gamma gamma\n"

	fetcher := &durableWebFetcher{runs: []durableWebRun{
		{pages: []websource.RemotePage{initial}, complete: true, state: json.RawMessage(`{"w":"1"}`)},
		{pages: []websource.RemotePage{changed}, complete: false, state: json.RawMessage(`{"w":"2"}`)},
		{pages: []websource.RemotePage{changed}, complete: false, state: json.RawMessage(`{"w":"2"}`)},
	}}
	fault := newFaultEmbeddingServer(t)
	m, store := newReconcileManagerWithDeps(t, fetcher, fault.server.URL, nil)
	if err := store.UpsertCollection(ctx, &websource.Collection{
		Name: "wiki", Type: "fake", Status: websource.StatusPending, Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	before, err := store.GetPage(ctx, websource.PageID("wiki", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	committedHash := before.Hash

	fault.arm()
	if err := m.RefreshWebSource(ctx, "wiki"); err == nil {
		t.Fatal("partial embedding failure was not returned")
	}
	partial, err := store.GetPage(ctx, websource.PageID("wiki", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if !partial.IndexDirty {
		t.Fatal("page was marked clean after a partial vector insert")
	}
	if partial.Hash != committedHash {
		t.Fatalf("committed hash advanced from %q to %q after partial indexing", committedHash, partial.Hash)
	}
	indexed, err := m.docs.rag.IndexedPaths(ctx, websource.ScopeKey("wiki"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := indexed["alpha.md"]; !ok {
		t.Fatal("fault did not leave a partial path in the vector index")
	}
	col, err := store.GetCollection(ctx, "wiki")
	if err != nil {
		t.Fatal(err)
	}
	if col.Status != websource.StatusError || col.LastError == "" {
		t.Fatalf("failed index status = %q, error = %q", col.Status, col.LastError)
	}
	if string(col.State) != `{"w":"1"}` {
		t.Fatalf("watermark advanced after partial indexing: %s", col.State)
	}

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("retry refresh: %v", err)
	}
	repaired, err := store.GetPage(ctx, websource.PageID("wiki", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if repaired.IndexDirty {
		t.Fatal("page remained dirty after successful retry")
	}
	if repaired.Hash == committedHash {
		t.Fatal("new content hash was not committed after successful retry")
	}
	col, err = store.GetCollection(ctx, "wiki")
	if err != nil {
		t.Fatal(err)
	}
	if string(col.State) != `{"w":"2"}` {
		t.Fatalf("watermark = %s, want retry state", col.State)
	}
	docs, err := m.docs.rag.Retrieve(ctx, vectorstore.FilterKey(websource.ScopeKey("wiki")), "gamma", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || !strings.Contains(docs[0].Excerpt, "gamma gamma") {
		t.Fatalf("tail chunks were not repaired: %+v", docs)
	}
}

func TestRefreshWebSourceRetriesStaleRowsAfterPartialRemoval(t *testing.T) {
	ctx := context.Background()
	fetcher := &durableWebFetcher{runs: []durableWebRun{
		{pages: []websource.RemotePage{remotePage("alpha"), remotePage("beta")}, complete: true, state: json.RawMessage(`{"w":"1"}`)},
		{complete: true, state: json.RawMessage(`{"w":"2"}`)},
		{complete: true, state: json.RawMessage(`{"w":"2"}`)},
	}}
	var fault *faultDeleteStore
	m, store := newReconcileManagerWithDeps(t, fetcher, "", func(base vectorstore.Store) vectorstore.Store {
		fault = &faultDeleteStore{Store: base}

		return fault
	})
	if err := store.UpsertCollection(ctx, &websource.Collection{
		Name: "wiki", Type: "fake", Status: websource.StatusPending, Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	fault.armed.Store(true)
	if err := m.RefreshWebSource(ctx, "wiki"); err == nil {
		t.Fatal("partial removal failure was not returned")
	}
	pages, err := store.Pages(ctx, "wiki")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 0 {
		t.Fatalf("removed page records survived: %+v", pages)
	}
	indexed, err := m.docs.rag.IndexedPaths(ctx, websource.ScopeKey("wiki"))
	if err != nil {
		t.Fatal(err)
	}
	if len(indexed) != 1 {
		t.Fatalf("partial removal left %d vector paths, want 1: %v", len(indexed), indexed)
	}
	col, err := store.GetCollection(ctx, "wiki")
	if err != nil {
		t.Fatal(err)
	}
	if col.Status != websource.StatusError || string(col.State) != `{"w":"1"}` {
		t.Fatalf("failed removal status=%q state=%s error=%q", col.Status, col.State, col.LastError)
	}

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("retry refresh: %v", err)
	}
	indexed, err = m.docs.rag.IndexedPaths(ctx, websource.ScopeKey("wiki"))
	if err != nil {
		t.Fatal(err)
	}
	if len(indexed) != 0 {
		t.Fatalf("stale vector paths survived retry: %v", indexed)
	}
	textIndexed, err := m.docsText.IndexedPaths(ctx, websource.ScopeKey("wiki"))
	if err != nil {
		t.Fatal(err)
	}
	if len(textIndexed) != 0 {
		t.Fatalf("stale text paths survived retry: %v", textIndexed)
	}
	col, err = store.GetCollection(ctx, "wiki")
	if err != nil {
		t.Fatal(err)
	}
	if col.Status != websource.StatusReady || string(col.State) != `{"w":"2"}` {
		t.Fatalf("repaired removal status=%q state=%s error=%q", col.Status, col.State, col.LastError)
	}
}
