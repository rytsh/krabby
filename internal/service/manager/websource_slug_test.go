package manager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/service/websource"
)

// TestDeleteWebPageRejectsTraversal is the regression test for an
// unauthenticated arbitrary-file delete. The REST handler takes the slug
// straight from a query parameter and it ends up in os.Remove as
// "<dir>/<slug>.md"; filepath.Join resolves ".." rather than rejecting it, so
// "../../x" escaped the collection directory and removed any *.md the process
// could reach.
func TestDeleteWebPageRejectsTraversal(t *testing.T) {
	ctx := context.Background()

	m := seedScripted(t, []scriptedRun{{pages: []websource.RemotePage{remotePage("alpha")}, complete: true}})

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// A file that only an escaping slug could reach: a sibling of the
	// collection directory, i.e. outside it.
	outside := filepath.Join(m.sourcesRootDir, "victim.md")
	if err := os.WriteFile(outside, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	for _, slug := range []string{"../victim", "../../victim", "..", "sub/alpha", "alpha/../../victim"} {
		if err := m.DeleteWebPage(ctx, "wiki", slug); err == nil {
			t.Errorf("slug %q was accepted", slug)
		}

		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("slug %q deleted a file outside the collection: %v", slug, err)
		}
	}

	// The legitimate case must still work.
	if err := m.DeleteWebPage(ctx, "wiki", "alpha"); err != nil {
		t.Fatalf("deleting a valid page: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.sourcesDir("wiki"), "alpha.md")); !os.IsNotExist(err) {
		t.Errorf("alpha.md survived its delete: %v", err)
	}
}

// TestSyncSkipsUnsafeProviderSlug covers the other direction: the slug comes
// from the remote provider, not the user. JIRA uses the raw issue key and
// Confluence the raw page id, so a hostile or broken instance can propose one.
// The bad page must be dropped without taking the rest of the sync with it, and
// without being recorded as seen (a "seen" phantom would later be pruned, and
// the prune is what performs the file delete).
func TestSyncSkipsUnsafeProviderSlug(t *testing.T) {
	ctx := context.Background()

	evil := remotePage("alpha")
	evil.Slug = "../../escape"

	m := seedScripted(t, []scriptedRun{{
		pages:    []websource.RemotePage{remotePage("alpha"), evil, remotePage("beta")},
		complete: true,
	}})

	outside := filepath.Join(m.sourcesRootDir, "escape.md")
	if err := os.WriteFile(outside, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a provider slug escaped the collection directory: %v", err)
	}

	// The well-formed pages either side of the bad one are still synced.
	got := syncedSlugs(t, m, "wiki")
	if !got["alpha"] || !got["beta"] {
		t.Errorf("one bad page aborted the sync: %v", got)
	}

	pages, err := m.webStore.Pages(ctx, "wiki")
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	for _, p := range pages {
		if !websource.ValidSlug(p.Slug) {
			t.Errorf("an unsafe slug was persisted: %q", p.Slug)
		}
	}
}

// TestFailedSyncStampsLastRefresh pins the backoff behaviour: the interval poll
// re-triggers anything whose LastRefreshAt is older than its interval and runs
// every minute, so a sync that fails without stamping the attempt is retried
// once a minute forever against a remote that is already refusing it.
func TestFailedSyncStampsLastRefresh(t *testing.T) {
	ctx := context.Background()

	// An empty script makes the second fetch fail ("unscripted fetch call").
	m := seedScripted(t, []scriptedRun{{pages: []websource.RemotePage{remotePage("alpha")}, complete: true}})

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := m.RefreshWebSource(ctx, "wiki"); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	col, err := m.webStore.GetCollection(ctx, "wiki")
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}

	if col.Status != websource.StatusError {
		t.Errorf("status = %q, want error", col.Status)
	}
	if col.LastRefreshAt.IsZero() {
		t.Error("a failed sync left LastRefreshAt unset, so the poll will retry it every minute")
	}
}

// TestReconcileInterruptedSyncsClearsFetching covers the restart case: the
// "fetching" status doubles as a skip flag for the interval poll, so a process
// killed mid-sweep would leave the collection parked forever.
func TestReconcileInterruptedSyncsClearsFetching(t *testing.T) {
	ctx := context.Background()

	m := seedScripted(t, nil)

	col, err := m.webStore.GetCollection(ctx, "wiki")
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	col.Status = websource.StatusFetching
	if err := m.webStore.UpsertCollection(ctx, col); err != nil {
		t.Fatalf("seed fetching status: %v", err)
	}

	if err := m.reconcileInterruptedSyncs(ctx); err != nil {
		t.Fatalf("reconcileInterruptedSyncs: %v", err)
	}

	col, err = m.webStore.GetCollection(ctx, "wiki")
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if col.Status == websource.StatusFetching {
		t.Error("a sync interrupted by a restart still claims to be fetching")
	}
	if col.LastRefreshAt.IsZero() == false {
		t.Error("an interrupted sync must not count as a completed attempt")
	}
}

func TestValidSlug(t *testing.T) {
	valid := []string{"alpha", "ofs-1234", "a.b_c-d", "123", "a"}
	invalid := []string{
		"", ".", "..", "../x", "a/b", "a\\b", "-leading", ".hidden",
		"UPPER", "with space", "emoji-🙂", string(make([]byte, 201)),
	}

	for _, s := range valid {
		if !websource.ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if websource.ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

func TestWithinDir(t *testing.T) {
	cases := []struct {
		root, path string
		want       bool
	}{
		{"/data/srcs", "/data/srcs", true},
		{"/data/srcs", "/data/srcs/wiki", true},
		{"/data/srcs", "/data/srcs-evil", false}, // the filepath.HasPrefix trap
		{"/data/srcs", "/data", false},
		{"/data/srcs", "/data/srcs/../other", false},
	}

	for _, c := range cases {
		if got := withinDir(c.root, c.path); got != c.want {
			t.Errorf("withinDir(%q, %q) = %v, want %v", c.root, c.path, got, c.want)
		}
	}
}

// seedScriptedJSON keeps the compiler honest about the config shape used by the
// scripted fixture above.
var _ = json.RawMessage(`{}`)

// blockingFetcher lets a test hold a sync open at a known point, so a
// concurrent config edit lands exactly in the window the sync spans.
type blockingFetcher struct {
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (f *blockingFetcher) Validate(json.RawMessage) error { return nil }

func (f *blockingFetcher) MergeConfig(_, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 {
		return json.RawMessage(`{}`), nil
	}

	return update, nil
}

func (f *blockingFetcher) ConfigView(json.RawMessage) any { return struct{}{} }

func (f *blockingFetcher) Fetch(_ context.Context, _ *websource.Collection, _ []*websource.Page, _ json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	if !f.once {
		f.once = true
		close(f.entered)
		<-f.release
	}

	if err := emit(remotePage("alpha")); err != nil {
		return nil, err
	}

	return &websource.FetchResult{Complete: true, State: json.RawMessage(`{"w":"1"}`)}, nil
}

// TestConfigEditDuringSyncSurvives is the regression test for a lost update.
// RefreshWebSource reads the collection at the top of a sweep and writes it
// back at the end; a sweep of a large project runs for minutes. Writing that
// stale snapshot back silently reverted any edit made in between — the UI had
// already shown the change as applied, because the handler read it back from
// its own successful write.
func TestConfigEditDuringSyncSurvives(t *testing.T) {
	ctx := context.Background()

	fetcher := &blockingFetcher{entered: make(chan struct{}), release: make(chan struct{})}

	m, webStore := newReconcileManager(t, fetcher)

	col := &websource.Collection{
		Name: "wiki", Type: "fake", Status: websource.StatusPending,
		Config: json.RawMessage(`{}`), Description: "before",
	}
	if err := webStore.UpsertCollection(ctx, col); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- m.RefreshWebSource(ctx, "wiki") }()

	<-fetcher.entered

	// The edit must not block behind the sweep either: the sync lock is held
	// for the whole run, so an update that waited on it would hang the request.
	edited := make(chan error, 1)
	go func() {
		update := websource.CollectionUpdate{Description: types.NewNull("after")}
		edited <- m.UpdateWebCollection(ctx, "wiki", update, nil)
	}()

	select {
	case err := <-edited:
		if err != nil {
			t.Fatalf("update during sync: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the config edit blocked behind the running sync")
	}

	close(fetcher.release)

	if err := <-done; err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got, err := webStore.GetCollection(ctx, "wiki")
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}

	if got.Description != "after" {
		t.Errorf("description = %q, want %q: the sync wrote its stale snapshot back", got.Description, "after")
	}

	// The sync's own fields still landed.
	if got.Status != websource.StatusReady {
		t.Errorf("status = %q, want ready", got.Status)
	}
	if len(got.State) == 0 {
		t.Error("the sync watermark was lost")
	}
}
