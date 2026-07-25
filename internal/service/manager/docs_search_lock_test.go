package manager

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/websource"
)

// gateFetcher blocks inside Fetch until released, so a test can hold a web
// source in the middle of a sync (with the per-scope lock taken) while it
// exercises the read path.
type gateFetcher struct {
	pages   []websource.RemotePage
	started chan struct{}
	release chan struct{}
	calls   int
}

func (f *gateFetcher) Validate(json.RawMessage) error { return nil }

func (f *gateFetcher) MergeConfig(_, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 {
		return json.RawMessage(`{}`), nil
	}

	return update, nil
}

func (f *gateFetcher) ConfigView(json.RawMessage) any { return struct{}{} }

func (f *gateFetcher) Fetch(_ context.Context, _ *websource.Collection, _ []*websource.Page, state json.RawMessage) (*websource.FetchResult, error) {
	f.calls++
	if f.calls > 1 {
		close(f.started)
		<-f.release
	}

	if len(state) != 0 {
		return &websource.FetchResult{Incremental: true, State: state}, nil
	}

	return &websource.FetchResult{Pages: f.pages, State: json.RawMessage(`{"w":"1"}`)}, nil
}

// TestSearchDocsDoesNotBlockOnRunningSync is the regression test for searches
// hanging while a web source syncs. SearchDocs warms the lexical index on
// demand, and that warm used to take the per-key lock unconditionally — the
// same lock RefreshWebSource holds for the whole fetch/write/index run. Any
// query touching the syncing key (including the default all-scope query, which
// walks every collection) blocked until the sync finished.
func TestSearchDocsDoesNotBlockOnRunningSync(t *testing.T) {
	ctx := context.Background()

	fetcher := &gateFetcher{
		pages: []websource.RemotePage{
			{Slug: "alpha", Title: "Alpha", URL: "https://x/alpha", Markdown: "# Alpha\n\nalpha alpha alpha\n"},
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}

	m, webStore := newReconcileManager(t, fetcher)

	col := &websource.Collection{
		Name: "wiki", Type: "fake", Status: websource.StatusPending, Config: json.RawMessage(`{}`),
	}
	if err := webStore.UpsertCollection(ctx, col); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	// First sync runs to completion: markdown on disk, both indexes populated.
	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	// Second sync parks inside Fetch while holding the "web:wiki" lock.
	synced := make(chan error, 1)
	go func() { synced <- m.RefreshWebSource(ctx, "wiki") }()

	select {
	case <-fetcher.started:
	case <-time.After(10 * time.Second):
		t.Fatal("sync never reached the fetch phase")
	}

	searched := make(chan []rag.Doc, 1)
	failed := make(chan error, 1)
	go func() {
		docs, err := m.SearchDocs(ctx, ScopeSources, "", registry.NamespaceAll, DocsSearchLexical, "alpha", 5)
		if err != nil {
			failed <- err

			return
		}
		searched <- docs
	}()

	select {
	case docs := <-searched:
		if len(docs) == 0 {
			t.Fatal("search returned no results for an already indexed source")
		}
	case err := <-failed:
		t.Fatalf("search during sync: %v", err)
	case <-time.After(5 * time.Second):
		close(fetcher.release)
		t.Fatal("search blocked on the in-flight sync instead of answering from the index")
	}

	close(fetcher.release)
	if err := <-synced; err != nil {
		t.Fatalf("second refresh: %v", err)
	}
}

// TestEnsureDocsTextKeySkipsLockedKey covers the other half: a key that was
// never lexically indexed but whose markdown is on disk. The backfill is
// best-effort, so when a write job owns the key the query must answer with
// what is indexed now rather than wait — the job indexes it on completion.
func TestEnsureDocsTextKeySkipsLockedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	text, err := rag.NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	docsRoot := t.TempDir()
	mustWriteManagerTest(t, filepath.Join(docsRoot, "acme", "payments", "incident.md"),
		"# PAY-1842\n\nGateway timeout ERR_CAPTURE_42")

	repo := &registry.Repo{ID: "acme/payments"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		reg:         reg,
		docsText:    text,
		docsRootDir: docsRoot,
		locks:       map[string]*sync.Mutex{},
		docs:        &docsBundle{},
	}

	// Simulate a refresh/generate holding the repo lock.
	lock := m.lock(repo.ID)
	lock.Lock()

	done := make(chan error, 1)
	go func() {
		_, err := m.SearchDocs(ctx, ScopeRepos, repo.ID, "", DocsSearchLexical, "PAY-1842", 5)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("search while key is locked: %v", err)
		}
	case <-time.After(5 * time.Second):
		lock.Unlock()
		t.Fatal("search blocked waiting for the key lock")
	}

	lock.Unlock()

	// Once the key is free the backfill runs and the same query resolves.
	docs, err := m.SearchDocs(ctx, ScopeRepos, repo.ID, "", DocsSearchLexical, "PAY-1842", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "incident.md" {
		t.Fatalf("docs after the lock was released = %#v", docs)
	}
}
