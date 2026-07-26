package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

// scriptedRun is one fetch a scriptedFetcher will perform, in order.
type scriptedRun struct {
	pages    []websource.RemotePage
	complete bool
	removed  []string
}

// scriptedFetcher plays a fixed sequence of fetches so a test can express
// exactly what a provider claimed on each sync.
type scriptedFetcher struct {
	runs []scriptedRun
	call int
}

func (f *scriptedFetcher) Validate(json.RawMessage) error { return nil }

func (f *scriptedFetcher) MergeConfig(_, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 {
		return json.RawMessage(`{}`), nil
	}

	return update, nil
}

func (f *scriptedFetcher) ConfigView(json.RawMessage) any { return struct{}{} }

func (f *scriptedFetcher) Fetch(_ context.Context, _ *websource.Collection, _ []*websource.Page, _ json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	if f.call >= len(f.runs) {
		return nil, fmt.Errorf("unscripted fetch call %d", f.call)
	}

	run := f.runs[f.call]
	f.call++

	for _, p := range run.pages {
		if err := emit(p); err != nil {
			return nil, err
		}
	}

	return &websource.FetchResult{
		Complete: run.complete,
		Removed:  run.removed,
		State:    json.RawMessage(`{"w":"1"}`),
	}, nil
}

func remotePage(slug string) websource.RemotePage {
	return websource.RemotePage{
		Slug:     slug,
		Title:    slug,
		URL:      "https://x/" + slug,
		Markdown: "# " + slug + "\n\nalpha beta gamma\n",
	}
}

// syncedSlugs is the set of slugs a collection currently holds, checked across
// all three places a page lives: the record store, the markdown on disk and the
// vector index. A page that survives in one but not the others is still a bug.
func syncedSlugs(t *testing.T, m *Manager, name string) map[string]bool {
	t.Helper()
	ctx := context.Background()

	pages, err := m.webStore.Pages(ctx, name)
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}

	indexed, err := m.docs.rag.IndexedPaths(ctx, websource.ScopeKey(name))
	if err != nil {
		t.Fatalf("IndexedPaths: %v", err)
	}

	out := map[string]bool{}
	for _, p := range pages {
		file := filepath.Join(m.sourcesDir(name), p.Slug+".md")
		if _, err := os.Stat(file); err != nil {
			t.Errorf("page %q has a record but no markdown on disk", p.Slug)

			continue
		}
		if _, ok := indexed[p.Slug+".md"]; !ok {
			t.Errorf("page %q has a record but no vectors", p.Slug)

			continue
		}
		out[p.Slug] = true
	}

	return out
}

func seedScripted(t *testing.T, runs []scriptedRun) *Manager {
	t.Helper()

	m, webStore := newReconcileManager(t, &scriptedFetcher{runs: runs})

	col := &websource.Collection{
		Name: "wiki", Type: "fake", Status: websource.StatusPending, Config: json.RawMessage(`{}`),
	}
	if err := webStore.UpsertCollection(context.Background(), col); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	return m
}

// TestTruncatedSweepDoesNotPrune is the regression test for the data loss a
// capped sync used to cause. A provider whose walk is cut short (JIRA's
// max_issues, Confluence's max_pages) returns a prefix of the collection while
// the run is still a full, non-incremental one. Reading that prefix as the
// whole inventory deletes every record past the cut — for a 35k-ticket project
// synced under a 5k cap, 30k tickets and their vectors — and the next sync
// re-fetches and re-embeds them, so the damage repeats on every full pass and
// shows up as a recurring embedding bill rather than an error.
func TestTruncatedSweepDoesNotPrune(t *testing.T) {
	ctx := context.Background()

	all := []websource.RemotePage{remotePage("alpha"), remotePage("beta"), remotePage("gamma")}

	m := seedScripted(t, []scriptedRun{
		// A complete sweep establishes all three pages.
		{pages: all, complete: true},
		// The next sweep is cut short by the cap: same kind of run, but it
		// only got as far as the first page and says so.
		{pages: all[:1], complete: false},
	})

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	if got := syncedSlugs(t, m, "wiki"); len(got) != 3 {
		t.Fatalf("after the complete sweep: %v, want all three pages", got)
	}

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	got := syncedSlugs(t, m, "wiki")
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		if !got[slug] {
			t.Errorf("page %q was pruned by a truncated sweep", slug)
		}
	}
}

// TestCompleteSweepPrunesVanished is the other half of the contract: the
// guarantee must still buy deletion when a provider really did enumerate
// everything, otherwise remotely-deleted pages would linger in search forever.
func TestCompleteSweepPrunesVanished(t *testing.T) {
	ctx := context.Background()

	all := []websource.RemotePage{remotePage("alpha"), remotePage("beta"), remotePage("gamma")}

	m := seedScripted(t, []scriptedRun{
		{pages: all, complete: true},
		// gamma is gone remotely, and this sweep saw the whole collection.
		{pages: all[:2], complete: true},
	})

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	got := syncedSlugs(t, m, "wiki")
	if got["gamma"] {
		t.Error("gamma survived a complete sweep that no longer lists it")
	}
	if !got["alpha"] || !got["beta"] {
		t.Errorf("a complete sweep pruned pages it still lists: %v", got)
	}

	// The markdown must go with the record, or the next reconcile would treat
	// the orphan file as a page needing re-embedding.
	if _, err := os.Stat(filepath.Join(m.sourcesDir("wiki"), "gamma.md")); !os.IsNotExist(err) {
		t.Errorf("gamma.md survived the prune: %v", err)
	}
}

// TestIncompleteSweepPrunesExplicitRemovals covers the third case: a provider
// that cannot enumerate everything may still know positively that an item is
// gone, and that knowledge is honoured independently of the sweep guarantee.
func TestIncompleteSweepPrunesExplicitRemovals(t *testing.T) {
	ctx := context.Background()

	all := []websource.RemotePage{remotePage("alpha"), remotePage("beta"), remotePage("gamma")}

	m := seedScripted(t, []scriptedRun{
		{pages: all, complete: true},
		{pages: nil, complete: false, removed: []string{"beta"}},
	})

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	got := syncedSlugs(t, m, "wiki")
	if got["beta"] {
		t.Error("an explicitly removed page survived")
	}
	if !got["alpha"] || !got["gamma"] {
		t.Errorf("an incomplete sweep pruned more than it reported: %v", got)
	}
}
