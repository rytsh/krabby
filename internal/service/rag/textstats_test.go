package rag

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rakunlabs/bw"
)

// newStatsCorpus indexes docs where "the" and "system" are in every document,
// "payment" in a fifth, and each doc has a unique term.
func newStatsCorpus(t *testing.T, docs int) *TextStore {
	t.Helper()

	store := newTestTextStore(t)

	recs := make([]*textRecord, 0, docs)
	for i := range docs {
		body := "the system handles it"
		if i%5 == 0 {
			body += " payment"
		}
		body += fmt.Sprintf(" unique%04d", i)

		recs = append(recs, &textRecord{
			ID:      fmt.Sprintf("r/%d.md#0", i),
			Repo:    "r",
			Path:    fmt.Sprintf("%d.md", i),
			Title:   "doc",
			Excerpt: body,
		})
	}

	if err := store.insert(context.Background(), recs); err != nil {
		t.Fatal(err)
	}

	return store
}

func TestRefreshStatsFindsCorpusWideTerms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newStatsCorpus(t, 200)

	if terms := store.FrequentTerms(ctx); terms != nil {
		t.Fatalf("frequent terms before RefreshStats = %#v, want none", terms)
	}

	if err := store.RefreshStats(ctx); err != nil {
		t.Fatal(err)
	}

	terms := store.FrequentTerms(ctx)
	for _, want := range []string{"the", "system", "handles"} {
		if !terms[want] {
			t.Errorf("corpus-wide term %q not detected: %#v", want, terms)
		}
	}
	// A fifth of the corpus is well under the threshold: domain vocabulary must
	// stay searchable.
	if terms["payment"] {
		t.Errorf("term present in 20%% of docs was dropped: %#v", terms)
	}
	if terms["unique0000"] {
		t.Errorf("a term unique to one document was dropped: %#v", terms)
	}
}

func TestRefreshStatsIsLanguageAgnostic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)

	// No built-in list covers these; they are found because the corpus shows
	// them to be everywhere.
	recs := make([]*textRecord, 0, 100)
	for i := range 100 {
		recs = append(recs, &textRecord{
			ID:      fmt.Sprintf("r/%d.md#0", i),
			Repo:    "r",
			Path:    fmt.Sprintf("%d.md", i),
			Title:   "belge",
			Excerpt: fmt.Sprintf("bu belge için ödeme ve iade kaydı %d numaralı konu%04d", i, i),
		})
	}
	if err := store.insert(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshStats(ctx); err != nil {
		t.Fatal(err)
	}

	terms := store.FrequentTerms(ctx)
	for _, want := range []string{"bu", "için", "ve"} {
		if !terms[want] {
			t.Errorf("corpus-wide Turkish term %q not detected: %#v", want, terms)
		}
	}
	if terms["konu0000"] {
		t.Errorf("a term unique to one document was dropped: %#v", terms)
	}
}

func TestRefreshStatsSkipsWhenCorpusIsStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newStatsCorpus(t, 200)

	if err := store.RefreshStats(ctx); err != nil {
		t.Fatal(err)
	}

	first, err := store.statsBucket.Get(ctx, docsStatsRecordID)
	if err != nil {
		t.Fatal(err)
	}

	// A small change must not trigger a recompute: which terms are corpus-wide
	// does not move, and the walk is not free.
	extra := []*textRecord{{ID: "r/extra.md#0", Repo: "r", Path: "extra.md", Title: "x", Excerpt: "the system"}}
	if err := store.insert(ctx, extra); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshStats(ctx); err != nil {
		t.Fatal(err)
	}

	second, err := store.statsBucket.Get(ctx, docsStatsRecordID)
	if err != nil {
		t.Fatal(err)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("stats recomputed for a %d -> %d document change", first.Total, second.Total)
	}
}

func TestRefreshStatsClearsOnEmptyCorpus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newStatsCorpus(t, 200)

	if err := store.RefreshStats(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.FrequentTerms(ctx)) == 0 {
		t.Fatal("expected frequent terms before clearing")
	}

	if err := store.DeleteRepo(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshStats(ctx); err != nil {
		t.Fatal(err)
	}

	if terms := store.FrequentTerms(ctx); terms != nil {
		t.Fatalf("frequent terms on an empty corpus = %#v, want none", terms)
	}

	if _, err := store.statsBucket.Get(ctx, docsStatsRecordID); !errors.Is(err, bw.ErrNotFound) {
		t.Fatalf("stats record after an empty corpus: err = %v, want ErrNotFound", err)
	}
}
