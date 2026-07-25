package coderag

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// chunkBytes is the size of one generated chunk, used to size the assertions
// against the corpus rather than against a magic number.
const chunkBytes = 44 * 60

// indexCorpus fills a text store with chunks spread over two repositories, so
// a repo-filtered search has to reject most of what the ranker returns.
func indexCorpus(t *testing.T, text *TextStore, perRepo int) {
	t.Helper()

	// A term every chunk contains, so a search for it matches the whole corpus
	// — the worst case for a filtered search, and a realistic one ("error",
	// "context", a common package name).
	body := strings.Repeat("handler context error retry timeout payload ", 60)
	if len(body) != chunkBytes {
		t.Fatalf("corpus chunk is %d bytes, the size constant says %d", len(body), chunkBytes)
	}

	for _, repo := range []string{"acme/app", "acme/other"} {
		items := make([]vectorstore.Item, 0, perRepo)
		for i := range perRepo {
			items = append(items, vectorstore.Item{
				ID: fmt.Sprintf("%s/file%04d.go#0", repo, i),
				Payload: vectorstore.Payload{
					Repo:      repo,
					DocPath:   fmt.Sprintf("file%04d.go", i),
					Symbol:    fmt.Sprintf("Handler%04d", i),
					StartLine: 1,
					EndLine:   40,
					Chunk:     body,
				},
			})
		}

		if err := text.InsertItems(t.Context(), items); err != nil {
			t.Fatalf("index %s: %v", repo, err)
		}
	}
}

// TestFilteredSearchPeakIsBoundedByThePage is the regression test for a search
// that held the whole index live while it ran. The filtered branch used to ask
// bw for every ranked hit at once — each carrying its full source snippet —
// build a second slice of the ones that passed the repository filter, and then
// take twenty rows out of it. Nothing leaked: both slices were garbage by the
// time the caller saw the result. But for the duration of the call the peak
// live heap grew with the corpus, and a handful of concurrent searches on a
// common term was a reliable way to OOM a container.
//
// So this measures neither what the call retains (a page, in both versions)
// nor what it allocates in total (every matching record is decoded either
// way), but how much is alive at once while it runs.
// Restricting a search to one repository must not, by itself, cost anything.
// Ranking is the floor: BM25 scores the whole matching corpus whether or not a
// filter is set, so the fair comparison is against the same query with no
// filter, on the same index. The old implementation hydrated every match on
// top of that floor and ran several times higher; the filter now costs a string
// compare per hit and should disappear into it.
func TestFilteredSearchCostsNoMoreThanRanking(t *testing.T) {
	for _, perRepo := range []int{1500, 6000} {
		t.Run(fmt.Sprintf("%d-documents", perRepo), func(t *testing.T) {
			text := newTextStore(t)
			indexCorpus(t, text, perRepo)

			const query = "handler context error"

			unfiltered := peakDuring(t, func() {
				if _, err := text.Search(t.Context(), "", query, 1, 20); err != nil {
					t.Fatal(err)
				}
			})

			var page SearchPage
			filtered := peakDuring(t, func() {
				var err error
				page, err = text.Search(t.Context(), "acme/app", query, 1, 20)
				if err != nil {
					t.Fatal(err)
				}
			})

			if len(page.Results) != 20 {
				t.Fatalf("got %d results, want a full page", len(page.Results))
			}
			if page.Total != uint64(perRepo) {
				t.Fatalf("Total = %d, want the exact %d", page.Total, perRepo)
			}

			t.Logf("corpus %s: unfiltered peak %s, filtered peak %s",
				human(uint64(perRepo)*chunkBytes*2), human(unfiltered), human(filtered))

			// Doubling the ranking floor leaves ample room for sampling noise
			// while still failing the old behaviour, which was several times
			// the floor and grew with the index.
			if limit := unfiltered*2 + (1 << 20); filtered > limit {
				t.Errorf("filtered search peaked at %s against a %s ranking floor "+
					"(limit %s); the filter is paying per hit again",
					human(filtered), human(unfiltered), human(limit))
			}
		})
	}
}

// TestFilteredSearchCountsEveryMatch pins what the pager depends on: the total
// is the true number of matches, however far past the page they run. A count
// that stopped at some ceiling would cap how deep the user could page and
// would misreport how much a query found.
func TestFilteredSearchCountsEveryMatch(t *testing.T) {
	for _, perRepo := range []int{400, 2500} {
		t.Run(fmt.Sprintf("%d-documents", perRepo), func(t *testing.T) {
			text := newTextStore(t)
			indexCorpus(t, text, perRepo)

			page, err := text.Search(t.Context(), "acme/app", "handler context error", 1, 20)
			if err != nil {
				t.Fatal(err)
			}

			if page.Total != uint64(perRepo) {
				t.Errorf("Total = %d, want the exact %d", page.Total, perRepo)
			}
			if len(page.Results) != 20 {
				t.Errorf("got %d results, want a full page", len(page.Results))
			}
		})
	}
}

// TestFilteredSearchPaginates makes sure the streaming rewrite still returns
// the right window, which the allocation assertions above would not catch.
func TestFilteredSearchPaginates(t *testing.T) {
	text := newTextStore(t)
	indexCorpus(t, text, 120)

	first, err := text.Search(t.Context(), "acme/app", "handler context error", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	second, err := text.Search(t.Context(), "acme/app", "handler context error", 2, 20)
	if err != nil {
		t.Fatal(err)
	}

	if first.Total != 120 || second.Total != 120 {
		t.Errorf("totals = %d and %d, want 120 on both pages", first.Total, second.Total)
	}
	if len(second.Results) != 20 {
		t.Fatalf("page 2 returned %d results, want 20", len(second.Results))
	}

	seen := map[string]struct{}{}
	for _, r := range first.Results {
		if r.Repo != "acme/app" {
			t.Fatalf("result from the wrong repository: %s", r.Repo)
		}
		seen[r.Path] = struct{}{}
	}
	for _, r := range second.Results {
		if r.Repo != "acme/app" {
			t.Fatalf("result from the wrong repository: %s", r.Repo)
		}
		if _, dup := seen[r.Path]; dup {
			t.Errorf("%s appears on both pages; the window is wrong", r.Path)
		}
	}

	// Past the end must be empty rather than an error or a wrapped window.
	beyond, err := text.Search(t.Context(), "acme/app", "handler context error", 99, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(beyond.Results) != 0 {
		t.Errorf("page 99 returned %d results, want none", len(beyond.Results))
	}
}

// peakDuring reports how far the live heap grew above its starting point while
// fn ran, by sampling from a second goroutine. Sampling is the only way to see
// a transient spike: it is gone by the time fn returns, so a before/after
// comparison reads zero whether or not the spike happened.
//
// The collector is deliberately left running. What is being measured is how
// much has to be alive at once, and garbage the collector can reclaim as it
// goes is precisely what does not have to be. Disabling it would turn this
// into a measurement of total allocation, under which streaming and
// hydrate-everything look identical.
func peakDuring(t *testing.T, fn func()) uint64 {
	t.Helper()

	runtime.GC()

	var start runtime.MemStats
	runtime.ReadMemStats(&start)

	stop := make(chan struct{})
	peak := make(chan uint64, 1)

	go func() {
		var (
			ms  runtime.MemStats
			top uint64
		)
		for {
			select {
			case <-stop:
				peak <- top

				return
			default:
			}

			// ReadMemStats stops the world, so sample at an interval rather
			// than in a tight loop: hammering it would slow the very call
			// being measured and change what it does.
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > top {
				top = ms.HeapAlloc
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	fn()

	close(stop)
	top := <-peak

	if top < start.HeapAlloc {
		return 0
	}

	return top - start.HeapAlloc
}

func human(n uint64) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
