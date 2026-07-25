package rag

import (
	"context"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// TestLexicalQueryMakesQuestionsSearchable pins the reason LexicalQuery
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

	built, err := store.Search(ctx, vectorstore.Filter{}, LexicalQuery(question, nil), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(built) == 0 || built[0].Path != "retry.md" {
		t.Fatalf("built query docs = %#v", built)
	}

	// A required identifier must still exclude documents that lack it.
	exact, err := store.Search(ctx, vectorstore.Filter{}, LexicalQuery("PAY-1842 gateway capture", nil), 5)
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
	docs, err := store.Search(ctx, vectorstore.Filter{}, LexicalQuery("How is the payment retry done?", nil), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 || docs[0].Path != "retry.md" {
		t.Fatalf("IDF did not out-rank the corpus-wide terms: %#v", docs)
	}
}

func TestLexicalQueryBuildsOrChain(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name, in, want string
		stop           StopWords
	}{
		{
			name: "prose words are ORed",
			in:   "payment retry loop",
			want: "payment OR retry OR loop",
		},
		{
			name: "no built-in list: function words survive by default",
			in:   "How does the payment retry work?",
			want: "how OR does OR the OR payment OR retry OR work",
		},
		{
			name: "configured stop words are dropped",
			in:   "How does the payment retry work?",
			stop: NewStopWords([]string{"how", "does", "the"}),
			want: "payment OR retry OR work",
		},
		{
			name: "stop words are matched case-insensitively",
			in:   "Wie ist der Zahlungsvorgang",
			stop: NewStopWords([]string{"Wie", "IST", "der"}),
			want: "zahlungsvorgang",
		},
		{
			name: "identifier stays required and is emitted after the OR chain",
			in:   "PAY-1842 gateway timeout",
			want: "gateway OR timeout PAY-1842",
		},
		{
			name: "error code and version are identifiers",
			in:   "ERR_CAPTURE_42 broke v0.39.0",
			want: "broke ERR_CAPTURE_42 v0.39.0",
		},
		{
			name: "single identifier has no OR chain",
			in:   "PAY-1842",
			want: "PAY-1842",
		},
		{
			name: "punctuation is trimmed",
			in:   "what is (retry), really?",
			want: "what OR is OR retry OR really",
		},
		{
			name: "duplicates collapse",
			in:   "retry retry Retry payment",
			want: "retry OR payment",
		},
		{
			name: "single-character words are dropped",
			in:   "a payment b retry",
			want: "payment OR retry",
		},
		{
			name: "everything filtered falls back to the original",
			in:   "how does it",
			stop: NewStopWords([]string{"how", "does", "it"}),
			want: "how does it",
		},
		{
			name: "empty stays empty",
			in:   "   ",
			want: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := LexicalQuery(tt.in, tt.stop); got != tt.want {
				t.Fatalf("LexicalQuery(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLexicalQueryPassesOperatorsThrough(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		`"gateway timeout"`,
		"payment OR refund",
		"payment NOT refund",
		"payment -refund",
		"retr*",
	} {
		if got := LexicalQuery(in, nil); got != in {
			t.Errorf("LexicalQuery(%q) = %q, want it forwarded verbatim", in, got)
		}
	}
}

// TestLexicalQueryBoundsTermCount pins the language-independent cost guard:
// every clause makes bw scan a posting list, so query length must be bounded
// even when no stop word list is configured.
func TestLexicalQueryBoundsTermCount(t *testing.T) {
	t.Parallel()

	words := []string{
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf",
		"hotel", "india", "juliett", "kilo", "lima", "mike", "november",
	}

	got := LexicalQuery(strings.Join(words, " "), nil)
	if n := strings.Count(got, " OR ") + 1; n != maxQueryTerms {
		t.Fatalf("LexicalQuery kept %d terms, want %d: %q", n, maxQueryTerms, got)
	}
}

func TestLexicalQueryKeepsRequiredTermsWithinBudget(t *testing.T) {
	t.Parallel()

	// Identifiers must survive even when prose fills the budget first.
	words := []string{
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf",
		"hotel", "india", "juliett", "kilo", "lima", "mike", "PAY-1842",
	}

	got := LexicalQuery(strings.Join(words, " "), nil)
	if !strings.Contains(got, "PAY-1842") {
		t.Fatalf("required identifier dropped: %q", got)
	}

	if strings.HasPrefix(got, "PAY-1842") {
		t.Fatalf("required term must follow the OR chain: %q", got)
	}
}

func TestNewStopWords(t *testing.T) {
	t.Parallel()

	if got := NewStopWords(nil); got != nil {
		t.Errorf("NewStopWords(nil) = %#v, want nil", got)
	}

	if got := NewStopWords([]string{"  ", ""}); got != nil {
		t.Errorf("NewStopWords(blank) = %#v, want nil", got)
	}

	got := NewStopWords([]string{"The", " AND ", "the"})
	if len(got) != 2 || !got["the"] || !got["and"] {
		t.Errorf("NewStopWords = %#v", got)
	}
}
