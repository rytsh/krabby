package rag

import (
	"context"
	"testing"

	"github.com/rytsh/krabby/internal/service/searchutil"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// TestLexicalQueryMakesQuestionsSearchable pins the reason searchutil.LexicalQuery
// exists: bw ANDs every term of a bare query, so a natural-language question
// matches nothing even when a document is obviously about it. Searching with
// the built query must find that document, and an exact key must stay exact.
func TestLexicalQueryMakesQuestionsSearchable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)

	docsDir := writeDocs(t, map[string]string{
		"retry.md":    "# Retry loop\n\nThe payment retry loop backs off exponentially.",
		"pay-1842.md": "# PAY-1842\n\nGateway capture failed with ERR_CONNECTION_RESET.",
	})
	if err := store.Index(ctx, "acme/payments", docsDir); err != nil {
		t.Fatal(err)
	}

	question := "How does the payment retry loop work?"

	raw, err := store.Search(ctx, vectorstore.Filter{}, question, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("bare question unexpectedly matched; the AND-semantics premise changed: %#v", raw)
	}

	built, err := store.Search(ctx, vectorstore.Filter{}, searchutil.LexicalQuery(question, nil), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(built) == 0 || built[0].Path != "retry.md" {
		t.Fatalf("built query docs = %#v", built)
	}

	// A required identifier must still exclude documents that lack it.
	exact, err := store.Search(ctx, vectorstore.Filter{}, searchutil.LexicalQuery("PAY-1842 gateway capture", nil), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 1 || exact[0].Path != "pay-1842.md" {
		t.Fatalf("required identifier did not constrain the query: %#v", exact)
	}
}

// TestLexicalQueryRanksWithoutStopWords documents why no built-in stop word
// list exists: BM25's IDF already drives a term present in every document to a
// near-zero contribution, in any language. Keeping the stop words therefore
// changes latency, not the ranking.
func TestLexicalQueryRanksWithoutStopWords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)

	docsDir := writeDocs(t, map[string]string{
		"retry.md":  "# Retry\n\nThe payment retry loop backs off.",
		"noise1.md": "# Noise one\n\nThe shipping label is printed.",
		"noise2.md": "# Noise two\n\nThe invoice is archived.",
		"noise3.md": "# Noise three\n\nThe report is generated.",
	})
	if err := store.Index(ctx, "acme/payments", docsDir); err != nil {
		t.Fatal(err)
	}

	// "the" and "is" appear in every document; "payment"/"retry" only in one.
	docs, err := store.Search(ctx, vectorstore.Filter{}, searchutil.LexicalQuery("How is the payment retry done?", nil), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 || docs[0].Path != "retry.md" {
		t.Fatalf("IDF did not out-rank the corpus-wide terms: %#v", docs)
	}
}
