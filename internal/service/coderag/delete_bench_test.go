package coderag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rakunlabs/bw"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func BenchmarkDeleteChangedPath(b *testing.B) {
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
	for start := 0; start < 5000; start += 100 {
		var items []vectorstore.Item
		for i := start; i < start+100; i++ {
			items = append(items, textItem("repo", fmt.Sprintf("file-%05d.go", i), "", 1, strings.Repeat("source line\n", 200)))
		}
		if err := s.InsertItems(ctx, items); err != nil {
			b.Fatal(err)
		}
	}
	target := []vectorstore.Item{textItem("repo", "changed.go", "", 1, "func changed() {}")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		if err := s.InsertItems(ctx, target); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := s.DeletePaths(ctx, "repo", []string{"changed.go"}); err != nil {
			b.Fatal(err)
		}
	}
}
