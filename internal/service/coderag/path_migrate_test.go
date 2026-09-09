package coderag

import (
	"context"
	"testing"

	"github.com/rakunlabs/bw"
)

type codeRecordBeforePathIndex struct {
	ID        string `bw:"id,pk"`
	Repo      string `bw:"repo,index"`
	Path      string `bw:"path,fts"`
	Symbol    string `bw:"symbol,fts"`
	StartLine int    `bw:"start_line"`
	EndLine   int    `bw:"end_line"`
	Snippet   string `bw:"snippet,fts,trigram"`
}

func TestPathIndexMigrationPreservesCodeSearch(t *testing.T) {
	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	old, err := bw.RegisterBucket[codeRecordBeforePathIndex](db, textBucketName, bw.WithVersion[codeRecordBeforePathIndex](2))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, rec := range []*codeRecordBeforePathIndex{
		{ID: "opaque-a", Repo: "a", Path: "main.go", StartLine: 1, Snippet: "needle alpha"},
		{ID: "opaque-b", Repo: "b", Path: "main.go", StartLine: 1, Snippet: "needle beta"},
		{ID: "opaque-c", Repo: "a", Path: "main.go.extra", StartLine: 1, Snippet: "needle gamma"},
	} {
		if err := old.Insert(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(ctx, "needle", TextSearchOptions{})
	if err != nil || page.Total != 3 {
		t.Fatalf("text page=%+v err=%v", page, err)
	}
	regex, err := s.SearchRegex(ctx, nil, "needle", RegexOptions{})
	if err != nil || regex.Total != 3 {
		t.Fatalf("regex page=%+v err=%v", regex, err)
	}
	if err := s.DeletePaths(ctx, "a", []string{"main.go", "main.go", "missing"}); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(ctx, "needle", TextSearchOptions{})
	if err != nil || page.Total != 2 {
		t.Fatalf("after delete=%+v err=%v", page, err)
	}
	for _, hit := range page.Results {
		if hit.Repo == "a" && hit.Path == "main.go" {
			t.Fatal("deleted record still searchable")
		}
	}
}
