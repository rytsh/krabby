package coderag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rakunlabs/bw"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func BenchmarkRegexPagedMatches(b *testing.B) {
	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	s, err := NewTextStore(db)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	for file := range 100 {
		var items []vectorstore.Item
		for chunk := range 10 {
			items = append(items, chunkItem("repo", fmt.Sprintf("file-%03d.go", file), "", chunk*50+1, chunk, strings.Repeat("needle := value\n", 50)))
		}
		if err := s.InsertItems(ctx, items); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		page, err := s.SearchRegex(ctx, nil, "needle", RegexOptions{PerPage: 10, MaxMatches: 20, ContextLines: 3})
		if err != nil || page.Total != 100 || len(page.Results) != 10 {
			b.Fatalf("total=%d results=%d err=%v", page.Total, len(page.Results), err)
		}
	}
}
