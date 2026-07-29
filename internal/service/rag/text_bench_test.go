package rag

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// benchCorpus builds a text index shaped like a large JIRA import: one big web
// source plus a handful of repositories, every document sharing the domain
// vocabulary a real query lands on.
func benchCorpus(tb testing.TB, tickets int) *TextStore {
	tb.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	store, err := NewTextStore(db)
	if err != nil {
		tb.Fatal(err)
	}

	records := make([]*textRecord, 0, tickets+200)
	for i := range tickets {
		records = append(records, &textRecord{
			ID:    fmt.Sprintf("web:jira/PAY-%d.md#0", i),
			Repo:  "web:jira",
			Path:  fmt.Sprintf("PAY-%d.md", i),
			Title: fmt.Sprintf("PAY-%d payment gateway timeout", i),
			Excerpt: fmt.Sprintf(
				"The payment gateway timed out during capture for order %d. "+
					"Retry the capture request and check the gateway timeout settings.", i),
			UpdatedAt: time.Now(),
		})
	}
	for i := range 200 {
		records = append(records, &textRecord{
			ID:      fmt.Sprintf("acme/payments/doc-%d.md#0", i),
			Repo:    "acme/payments",
			Path:    fmt.Sprintf("doc-%d.md", i),
			Title:   fmt.Sprintf("Gateway client %d", i),
			Excerpt: "The payment gateway client retries a capture after a timeout.",
		})
	}

	if err := store.insert(context.Background(), records); err != nil {
		tb.Fatal(err)
	}

	return store
}

// BenchmarkTextStoreSearch measures the shape that used to hang: a filtered
// query whose terms match the whole corpus.
func BenchmarkTextStoreSearch(b *testing.B) {
	for _, tickets := range []int{2000, 10000} {
		store := benchCorpus(b, tickets)
		ctx := context.Background()

		for _, tc := range []struct {
			name   string
			filter vectorstore.Filter
		}{
			{name: "unfiltered"},
			{name: "one_source", filter: vectorstore.FilterKey("web:jira")},
			// The pathological case: the filter selects the small partition
			// while the ranking is dominated by the large one.
			{name: "small_repo", filter: vectorstore.FilterKey("acme/payments")},
			{name: "repos_only", filter: vectorstore.Filter{Kind: vectorstore.KindRepo}},
		} {
			b.Run(fmt.Sprintf("tickets=%d/%s", tickets, tc.name), func(b *testing.B) {
				for b.Loop() {
					docs, err := store.Search(ctx, tc.filter, "payment gateway timeout capture", 5)
					if err != nil {
						b.Fatal(err)
					}
					if len(docs) == 0 {
						b.Fatal("no documents matched; the benchmark is not measuring a real query")
					}
				}
			})
		}
	}
}

// BenchmarkTextStoreHasRepo measures the existence probe the docs search runs
// for every key in scope before it can answer.
func BenchmarkTextStoreHasRepo(b *testing.B) {
	store := benchCorpus(b, 10000)
	ctx := context.Background()

	for b.Loop() {
		if _, err := store.HasRepo(ctx, "acme/payments"); err != nil {
			b.Fatal(err)
		}
	}
}
