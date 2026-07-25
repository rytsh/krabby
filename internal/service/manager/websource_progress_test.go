package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/websource"
)

// reportingFetcher pages through a fixed result set and reports discovery
// progress the way the JIRA and Confluence fetchers do. It captures the
// progress the manager published mid-fetch so the test can assert on it.
type reportingFetcher struct {
	pages []websource.RemotePage

	mu       sync.Mutex
	observed []Progress
	observe  func() (Progress, bool)
}

func (f *reportingFetcher) Validate(json.RawMessage) error { return nil }

func (f *reportingFetcher) MergeConfig(_, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 {
		return json.RawMessage(`{}`), nil
	}

	return update, nil
}

func (f *reportingFetcher) ConfigView(json.RawMessage) any { return struct{}{} }

func (f *reportingFetcher) Fetch(ctx context.Context, _ *websource.Collection, _ []*websource.Page, _ json.RawMessage) (*websource.FetchResult, error) {
	for i := range f.pages {
		progress.Report(ctx, i+1, len(f.pages))

		if f.observe != nil {
			if p, ok := f.observe(); ok {
				f.mu.Lock()
				f.observed = append(f.observed, p)
				f.mu.Unlock()
			}
		}
	}

	return &websource.FetchResult{Pages: f.pages}, nil
}

// TestRefreshWebSourcePublishesFetchProgress checks the fetch phase is
// determinate for providers that know their result-set size: a 30k-ticket JIRA
// sync should show "1200/30000", not an open-ended spinner.
func TestRefreshWebSourcePublishesFetchProgress(t *testing.T) {
	ctx := context.Background()

	var pages []websource.RemotePage
	for i := range 6 {
		pages = append(pages, websource.RemotePage{
			Slug:     fmt.Sprintf("page-%d", i),
			Title:    fmt.Sprintf("Page %d", i),
			URL:      fmt.Sprintf("https://x/%d", i),
			Markdown: fmt.Sprintf("# Page %d\n\nalpha beta\n", i),
		})
	}

	fetcher := &reportingFetcher{pages: pages}

	m, webStore := newReconcileManager(t, fetcher)
	fetcher.observe = func() (Progress, bool) {
		// A sync runs one phase at a time, so anything else is a bug in the
		// phase handover.
		phases, ok := m.Progress(websource.ScopeKey("wiki"))
		if !ok || len(phases) != 1 {
			return Progress{}, false
		}

		return phases[0], true
	}

	col := &websource.Collection{
		Name: "wiki", Type: "fake", Status: websource.StatusPending, Config: json.RawMessage(`{}`),
	}
	if err := webStore.UpsertCollection(ctx, col); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	if err := m.RefreshWebSource(ctx, "wiki"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	fetcher.mu.Lock()
	observed := append([]Progress(nil), fetcher.observed...)
	fetcher.mu.Unlock()

	if len(observed) != len(pages) {
		t.Fatalf("observed %d progress samples, want %d", len(observed), len(pages))
	}

	for i, p := range observed {
		if p.Phase != "fetch" {
			t.Fatalf("sample %d phase = %q, want fetch", i, p.Phase)
		}
		if p.Done != i+1 || p.Total != len(pages) {
			t.Fatalf("sample %d = %d/%d, want %d/%d", i, p.Done, p.Total, i+1, len(pages))
		}
		if p.StartedAt.IsZero() {
			t.Fatalf("sample %d carries no phase start, so no estimate is possible", i)
		}
	}

	// The phase clock must span the whole fetch, not restart per report.
	if !observed[0].StartedAt.Equal(observed[len(observed)-1].StartedAt) {
		t.Fatal("the fetch phase clock restarted between progress reports")
	}

	// Progress is transient: it is gone once the sync finishes.
	if p, ok := m.Progress(websource.ScopeKey("wiki")); ok {
		t.Fatalf("progress survived the sync: %#v", p)
	}
}
