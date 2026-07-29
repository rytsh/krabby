package rag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func newTestTextStore(t *testing.T) *TextStore {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return store
}

func TestTextStoreSearchExactTermsAndFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)

	jiraDir := writeDocs(t, map[string]string{
		"pay-1842.md": "# PAY-1842 Checkout timeout\n\nThe payment gateway returns ERR_CONNECTION_RESET during capture.",
		"pay-2000.md": "# PAY-2000 Receipt copy\n\nCustomers need a second receipt.",
	})
	repoDir := writeDocs(t, map[string]string{
		"runbook.md": "# Checkout runbook\n\nRestart the payment worker after a timeout.",
	})

	if err := store.Index(ctx, "web:jira", jiraDir); err != nil {
		t.Fatal(err)
	}
	if err := store.Index(ctx, "acme/payments", repoDir); err != nil {
		t.Fatal(err)
	}

	docs, err := store.Search(ctx, vectorstore.FilterKey("web:jira"), "PAY-1842", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "pay-1842.md" {
		t.Fatalf("jira key search = %#v", docs)
	}

	docs, err = store.Search(ctx, vectorstore.Filter{Kind: vectorstore.KindWeb}, "ERR_CONNECTION_RESET", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Repo != "web:jira" {
		t.Fatalf("web-source filter search = %#v", docs)
	}

	docs, err = store.Search(ctx, vectorstore.Filter{Kind: vectorstore.KindRepo}, "checkout", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Repo != "acme/payments" {
		t.Fatalf("repo-only filter search = %#v", docs)
	}
}

// TestTextStoreSearchFiltersDeepInTheRanking is the regression test for the
// docs search that hung on a large corpus. The filter selects a small
// partition while a much bigger one dominates the ranking, so every matching
// document of the target repo sits far below the cut of any fixed-size page.
//
// The previous implementation paged through the ranked hits in fixed batches,
// re-evaluating the whole query for every page, which made this shape
// quadratic in corpus size. The behavioural contract it must still satisfy is
// simply that the results are complete and correctly ordered.
func TestTextStoreSearchFiltersDeepInTheRanking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)

	// A large web source whose documents match the query terms repeatedly, so
	// they take every top rank.
	bulk := make([]*textRecord, 0, 1200)
	for i := range 1200 {
		bulk = append(bulk, &textRecord{
			ID:      fmt.Sprintf("web:jira/PAY-%d.md#0", i),
			Repo:    "web:jira",
			Path:    fmt.Sprintf("PAY-%d.md", i),
			Title:   fmt.Sprintf("PAY-%d payment gateway timeout capture", i),
			Excerpt: strings.Repeat("payment gateway timeout capture. ", 4),
		})
	}
	// Two documents in a small repository. They match the same terms but only
	// once each, in a longer text, so BM25 ranks them below the whole bulk.
	padding := strings.Repeat("unrelated background prose. ", 40)
	bulk = append(bulk,
		&textRecord{
			ID:      "acme/payments/runbook.md#0",
			Repo:    "acme/payments",
			Path:    "runbook.md",
			Title:   "Runbook",
			Excerpt: "Restart the worker: payment gateway timeout on capture. " + padding,
		},
		&textRecord{
			ID:      "acme/payments/design.md#0",
			Repo:    "acme/payments",
			Path:    "design.md",
			Title:   "Design",
			Excerpt: "Capture path: payment gateway timeout handling. " + padding + padding,
		},
	)
	if err := store.insert(ctx, bulk); err != nil {
		t.Fatal(err)
	}

	docs, err := store.Search(ctx, vectorstore.FilterKey("acme/payments"), "payment gateway timeout capture", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("filtered search found %d of 2 documents: %#v", len(docs), docs)
	}
	for _, doc := range docs {
		if doc.Repo != "acme/payments" {
			t.Fatalf("filter leaked %q", doc.Repo)
		}
	}
	if docs[0].Score < docs[1].Score {
		t.Fatalf("results are not ordered by score: %#v", docs)
	}
}

// TestTextStoreSearchCountsDocumentsNotChunks pins that topDocs is a document
// budget: the index stores one record per chunk, so a document with many
// chunks must not consume several slots.
func TestTextStoreSearchCountsDocumentsNotChunks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)

	var records []*textRecord
	for doc := range 4 {
		for chunk := range 5 {
			records = append(records, &textRecord{
				ID:      fmt.Sprintf("web:wiki/doc-%d.md#%d", doc, chunk),
				Repo:    "web:wiki",
				Path:    fmt.Sprintf("doc-%d.md", doc),
				Title:   fmt.Sprintf("Doc %d", doc),
				Excerpt: fmt.Sprintf("chunk %d of the deployment runbook", chunk),
			})
		}
	}
	if err := store.insert(ctx, records); err != nil {
		t.Fatal(err)
	}

	docs, err := store.Search(ctx, vectorstore.FilterKey("web:wiki"), "deployment runbook", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("want 3 distinct documents, got %d: %#v", len(docs), docs)
	}

	seen := map[string]bool{}
	for _, doc := range docs {
		if seen[doc.Path] {
			t.Fatalf("the same document was returned twice: %#v", docs)
		}
		seen[doc.Path] = true
	}
}

// TestTextStoreSearchStopsOnCancelledContext checks that an abandoned query
// (a cancelled HTTP request, say) does not run to completion.
func TestTextStoreSearchStopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)

	var records []*textRecord
	for i := range 3000 {
		records = append(records, &textRecord{
			ID:      fmt.Sprintf("web:wiki/doc-%d.md#0", i),
			Repo:    "web:wiki",
			Path:    fmt.Sprintf("doc-%d.md", i),
			Title:   fmt.Sprintf("Doc %d", i),
			Excerpt: "deployment runbook rollback procedure",
		})
	}
	if err := store.insert(ctx, records); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := store.Search(cancelled, vectorstore.Filter{}, "deployment runbook rollback procedure", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search error = %v, want context.Canceled", err)
	}
}

func TestTextStoreIndexPathsUpdatesAndRemoves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)
	dir := writeDocs(t, map[string]string{
		"keep.md": "# Keep\n\nlegacy phrase",
		"drop.md": "# Drop\n\nobsolete ticket",
	})

	if err := store.Index(ctx, "web:wiki", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.md"), []byte("# Keep\n\ncurrent phrase"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "drop.md")); err != nil {
		t.Fatal(err)
	}

	updated := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	err := store.IndexPaths(ctx, "web:wiki", dir, []string{"keep.md"}, []string{"drop.md"}, &IndexOptions{
		UpdatedAt: func(string) time.Time { return updated },
	})
	if err != nil {
		t.Fatal(err)
	}

	oldDocs, err := store.Search(ctx, vectorstore.FilterKey("web:wiki"), "legacy", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldDocs) != 0 {
		t.Fatalf("stale changed text remains: %#v", oldDocs)
	}
	dropped, err := store.Search(ctx, vectorstore.FilterKey("web:wiki"), "obsolete", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("removed path remains: %#v", dropped)
	}
	current, err := store.Search(ctx, vectorstore.FilterKey("web:wiki"), "current", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || !current[0].UpdatedAt.Equal(updated) {
		t.Fatalf("updated path = %#v", current)
	}
}

// TestDocsTextMigrateV1ToV2 guards the lexical index's half of the scope-filter
// change.
//
// The vector and lexical arms answer the same scope question, so both had to
// gain the Kind discriminator. A missed backfill here would be quiet in the
// worst way: hybrid search would still return results from the semantic arm,
// just fewer and worse, with nothing in the logs to say why.
func TestDocsTextMigrateV1ToV2(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Seed in the pre-Kind shape.
	old, err := bw.RegisterBucket[textRecordV1](db, docsTextBucketName, bw.WithVersion[textRecordV1](1))
	if err != nil {
		t.Fatal(err)
	}

	seeded := []*textRecordV1{
		{ID: "acme/payments/runbook.md#0", Repo: "acme/payments", Path: "runbook.md", Title: "Checkout runbook", Excerpt: "restart the payment worker after a timeout"},
		{ID: "web:jira/pay-1842.md#0", Repo: "web:jira", Path: "pay-1842.md", Title: "PAY-1842", Excerpt: "the payment gateway returns a connection reset"},
	}
	for _, rec := range seeded {
		if err := old.Insert(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	// Pad the bucket with documents whose vocabulary is wide enough that a
	// full batch of them cannot commit in one transaction. A full-text write
	// costs one key per distinct term, so a real docs corpus reaches that
	// limit easily — and a migration that cannot batch around it fails
	// half-way, leaving the index in a state no retry recovers from.
	for i := range 200 {
		var sb strings.Builder
		for w := range 3000 {
			fmt.Fprintf(&sb, "d%dt%d ", i, w)
		}

		rec := &textRecordV1{
			ID:      fmt.Sprintf("acme/bulk/doc%03d.md#0", i),
			Repo:    "acme/bulk",
			Path:    fmt.Sprintf("doc%03d.md", i),
			Title:   "bulk",
			Excerpt: sb.String(),
		}
		if err := old.Insert(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	// Registering the current shape runs the migration.
	store, err := NewTextStore(db)
	if err != nil {
		t.Fatalf("migrating the docs text bucket: %v", err)
	}

	for _, tc := range []struct {
		name     string
		filter   vectorstore.Filter
		wantPath string
	}{
		{"repo scope", vectorstore.Filter{Kind: vectorstore.KindRepo}, "runbook.md"},
		{"web scope", vectorstore.Filter{Kind: vectorstore.KindWeb}, "pay-1842.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := store.Search(ctx, tc.filter, "payment", 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(docs) != 1 {
				t.Fatalf("got %d docs, want 1 — Kind was not backfilled: %#v", len(docs), docs)
			}
			if docs[0].Path != tc.wantPath {
				t.Fatalf("got %q, want %q", docs[0].Path, tc.wantPath)
			}
		})
	}
}
