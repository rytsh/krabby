package coderag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// uiResultKey mirrors resultKey() in _ui/src/routes/Search.svelte for a normal
// code hit. Svelte throws on a duplicate key in a keyed each block, which aborts
// the render mid-update and leaves the page on its previous state.
func uiResultKey(s Snippet) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", s.Repo, s.Path, s.StartLine, s.EndLine, s.Line)
}

func TestSearchResultsAreUniquePerChunk(t *testing.T) {
	// A file long enough to chunk, with the identifier on a line that lands in
	// the overlap two consecutive chunks share.
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		if i == 60 {
			b.WriteString("\tmetrics.batch_count.Inc()\n")
			continue
		}
		fmt.Fprintf(&b, "\tvalue%d := compute(%d)\n", i, i)
	}

	chunks := chunkFile(b.String(), nil, 900, 300)
	t.Logf("chunks: %d", len(chunks))
	for _, c := range chunks {
		if c.StartLine <= 60 && 60 <= c.EndLine {
			t.Logf("chunk covering line 60: %d-%d", c.StartLine, c.EndLine)
		}
	}

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	items := make([]vectorstore.Item, 0, len(chunks))
	for i, c := range chunks {
		items = append(items, vectorstore.Item{
			ID: fmt.Sprintf("acme/svc/svc.go#%d", i),
			Payload: vectorstore.Payload{
				Repo: "acme/svc", DocPath: "svc.go", Symbol: c.Symbol,
				StartLine: c.StartLine, EndLine: c.EndLine, Chunk: c.Text,
			},
		})
	}
	ctx := context.Background()
	if err := s.ReplaceRepo(ctx, "acme/svc", items); err != nil {
		t.Fatal(err)
	}

	page, err := s.Search(ctx, "batch_count", TextSearchOptions{PerPage: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("results: %d", len(page.Results))

	seen := map[string]Snippet{}
	for _, r := range page.Results {
		t.Logf("  repo=%s path=%s start_line=%d line=%d", r.Repo, r.Path, r.StartLine, r.Line)
		k := uiResultKey(r)
		if prev, dup := seen[k]; dup {
			t.Fatalf("duplicate UI key %q: chunks starting at %d and %d both report line %d",
				strings.ReplaceAll(k, "\x00", "|"), prev.StartLine, r.StartLine, r.Line)
		}
		seen[k] = r
	}
}
