package coderag

import (
	"context"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func TestTextStoreSearchPaginationAndRepoFilter(t *testing.T) {
	t.Parallel()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "retry.go", "RetryRequest", 10, "func RetryRequest() {\n\tretry failed request with backoff\n}"),
		textItem("acme/api", "worker.go", "RetryJob", 20, "retry queued job with delay"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepo(ctx, "acme/web", []vectorstore.Item{
		textItem("acme/web", "client.go", "RetryFetch", 30, "retry browser fetch after failure"),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := store.Search(ctx, "retry", coderagOpts(1, 2, ""))
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || len(first.Results) != 2 || first.Page != 1 || first.PerPage != 2 {
		t.Fatalf("first page = %#v, want total=3 and 2 results", first)
	}

	second, err := store.Search(ctx, "retry", coderagOpts(2, 2, ""))
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 3 || len(second.Results) != 1 {
		t.Fatalf("second page = %#v, want total=3 and 1 result", second)
	}

	filtered, err := store.Search(ctx, "retry", coderagOpts(1, 1, "acme/api"))
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 2 || len(filtered.Results) != 1 || filtered.Results[0].Repo != "acme/api" {
		t.Fatalf("filtered page = %#v, want exact repo total=2", filtered)
	}

	bySymbol, err := store.Search(ctx, "RetryFetch", coderagOpts(1, 20, ""))
	if err != nil {
		t.Fatal(err)
	}
	if bySymbol.Total != 1 || bySymbol.Results[0].Path != "client.go" {
		t.Fatalf("symbol search = %#v", bySymbol)
	}

	exactLine, err := store.Search(ctx, "failed request", coderagOpts(1, 20, "acme/api"))
	if err != nil {
		t.Fatal(err)
	}
	if exactLine.Total != 1 || exactLine.Results[0].Line != 11 {
		t.Fatalf("exact match line = %#v, want line 11", exactLine)
	}
}

func TestTextStoreReplaceAndDeleteRepo(t *testing.T) {
	t.Parallel()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "old.go", "Old", 1, "legacy retry implementation"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "new.go", "New", 1, "circuit breaker implementation"),
	}); err != nil {
		t.Fatal(err)
	}

	old, err := store.Search(ctx, "legacy", coderagOpts(1, 20, ""))
	if err != nil {
		t.Fatal(err)
	}
	if old.Total != 0 {
		t.Fatalf("stale chunks remain after replacement: %#v", old)
	}

	hasRepo, err := store.HasRepo(ctx, "acme/api")
	if err != nil || !hasRepo {
		t.Fatalf("HasRepo = %v, %v", hasRepo, err)
	}
	if err := store.DeleteRepo(ctx, "acme/api"); err != nil {
		t.Fatal(err)
	}

	current, err := store.Search(ctx, "circuit", coderagOpts(1, 20, ""))
	if err != nil {
		t.Fatal(err)
	}
	if current.Total != 0 {
		t.Fatalf("chunks remain after delete: %#v", current)
	}
}

func TestTextStoreDeletePaths(t *testing.T) {
	t.Parallel()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "keep.go", "Keep", 1, "keeper implementation"),
		textItem("acme/api", "drop.go", "Drop", 1, "dropper implementation"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.DeletePaths(ctx, "acme/api", []string{"drop.go", "missing.go"}); err != nil {
		t.Fatal(err)
	}

	dropped, err := store.Search(ctx, "dropper", coderagOpts(1, 20, ""))
	if err != nil {
		t.Fatal(err)
	}
	if dropped.Total != 0 {
		t.Fatalf("dropped path still searchable: %#v", dropped)
	}

	kept, err := store.Search(ctx, "keeper", coderagOpts(1, 20, ""))
	if err != nil {
		t.Fatal(err)
	}
	if kept.Total != 1 || kept.Results[0].Path != "keep.go" {
		t.Fatalf("kept path lost: %#v", kept)
	}
}

func textItem(repo, path, symbol string, line int, snippet string) vectorstore.Item {
	return vectorstore.Item{
		ID: repo + "/" + path + "#0",
		Payload: vectorstore.Payload{
			Repo:      repo,
			DocPath:   path,
			Symbol:    symbol,
			StartLine: line,
			EndLine:   line + 2,
			Chunk:     snippet,
		},
	}
}

// coderagOpts builds search options for the tests, scoping to one repository
// the same way the manager does: through the chunk-id key filter.
func coderagOpts(page, perPage int, repo string) TextSearchOptions {
	opts := TextSearchOptions{Page: page, PerPage: perPage}
	if repo != "" {
		filter, err := Scope{Repos: []string{repo}}.KeyFilter()
		if err != nil {
			panic(err)
		}
		opts.KeyFilter = filter
	}

	return opts
}
