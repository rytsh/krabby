package rag

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// benchQuestion is a typical natural-language docs question: a few function
// words and a few domain terms.
const benchQuestion = "How does the payment retry gateway handle a capture timeout?"

// buildBenchCorpus fills a TextStore with n synthetic chunks.
//
// The vocabulary is Zipfian on purpose. Real prose has a handful of function
// words in nearly every document and a long tail of rare content words, and
// lexical query time is linear in how many documents a term matches, so a flat
// vocabulary would make every term behave like "the" and misreport the cost of
// a query by an order of magnitude.
func buildBenchCorpus(tb testing.TB, n int) *TextStore {
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

	const vocabSize = 4000

	vocab := make([]string, 0, vocabSize)
	for i := range vocabSize {
		vocab = append(vocab, fmt.Sprintf("term%04d", i))
	}
	// The question's domain terms, planted at mid/low ranks so they land in a
	// few percent of documents like real domain vocabulary.
	vocab[40], vocab[90] = "payment", "retry"
	vocab[300], vocab[700], vocab[1500] = "gateway", "capture", "timeout"

	rng := rand.New(rand.NewSource(1))
	zipf := rand.NewZipf(rng, 1.2, 1, vocabSize-1)
	filler := "the and of to in is that it for on with as this an be by "

	recs := make([]*textRecord, 0, n)
	for i := range n {
		body := filler
		for range 120 {
			body += vocab[zipf.Uint64()] + " "
		}

		recs = append(recs, &textRecord{
			ID:      fmt.Sprintf("r/%d.md#0", i),
			Repo:    "r",
			Path:    fmt.Sprintf("%d.md", i),
			Title:   fmt.Sprintf("Document %d", i),
			Excerpt: body,
		})
	}

	if err := store.insert(context.Background(), recs); err != nil {
		tb.Fatal(err)
	}

	return store
}

// BenchmarkLexicalSearch compares the lexical arm with and without the
// corpus-derived frequent-term filter. Both must return the same documents;
// the filter only removes terms that match most of the index.
func BenchmarkLexicalSearch(b *testing.B) {
	ctx := context.Background()

	for _, n := range []int{10000, 40000} {
		store := buildBenchCorpus(b, n)
		if err := store.RefreshStats(ctx); err != nil {
			b.Fatal(err)
		}

		for _, tt := range []struct {
			name string
			stop StopWords
		}{
			{"unfiltered", nil},
			{"frequent-filtered", store.FrequentTerms(ctx)},
		} {
			query := LexicalQuery(benchQuestion, tt.stop)

			b.Run(fmt.Sprintf("n=%d/%s", n, tt.name), func(b *testing.B) {
				for b.Loop() {
					if _, err := store.Search(ctx, vectorstore.Filter{}, query, 12); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkRefreshStats measures the index-time cost of deriving the
// frequent-term set. It runs after an index rebuild, never on a query.
func BenchmarkRefreshStats(b *testing.B) {
	ctx := context.Background()
	store := buildBenchCorpus(b, 40000)

	b.ResetTimer()
	for b.Loop() {
		// Drop the record so the staleness guard does not short-circuit.
		b.StopTimer()
		_ = store.statsBucket.Delete(ctx, docsStatsRecordID)
		b.StartTimer()

		if err := store.RefreshStats(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLexicalQueryBuild(b *testing.B) {
	stop := NewStopWords([]string{"how", "does", "the", "a", "is"})

	for b.Loop() {
		_ = LexicalQuery(benchQuestion, stop)
	}
}
