package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func (f *fakeReconcileFetcher) Fetch(_ context.Context, _ *websource.Collection, _ []*websource.Page, state json.RawMessage) (*websource.FetchResult, error) {
	if len(state) != 0 {
		// Incremental run: nothing changed since the stored watermark.
		return &websource.FetchResult{Incremental: true, State: state}, nil
	}

	// First, full discovery run.
	return &websource.FetchResult{
		Pages: f.pages,
		State: json.RawMessage(`{"w":"1"}`),
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
	t.Helper()
	ctx := context.Background()

	emb, err := embedder.New(config.Embedder{BaseURL: fakeReconcileEmbedServer(t).URL, Model: "fake"})
	if err != nil {
		t.Fatalf("embedder.New: %v", err)
	}

	store, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("vectorstore.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

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
