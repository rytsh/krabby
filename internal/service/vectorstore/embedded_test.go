package vectorstore

import (
	"context"
	"sync"
	"testing"
)

func testItems() []Item {
	return []Item{
		{
			ID:      "o/a/doc.md#0",
			Vector:  []float32{1, 0, 0},
			Payload: Payload{Repo: "o/a", DocPath: "doc.md", Title: "A", Chunk: "alpha"},
		},
		{
			ID:      "o/a/doc.md#1",
			Vector:  []float32{0.9, 0.1, 0},
			Payload: Payload{Repo: "o/a", DocPath: "doc.md", Title: "A", Chunk: "alpha 2"},
		},
		{
			ID:      "o/b/doc.md#0",
			Vector:  []float32{0, 1, 0},
			Payload: Payload{Repo: "o/b", DocPath: "doc.md", Title: "B", Chunk: "beta"},
		},
	}
}

func openEmbedded(t *testing.T, dir string) *embedded {
	t.Helper()

	s, err := newEmbedded(dir)
	if err != nil {
		t.Fatalf("newEmbedded: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestEmbeddedSearchRanksAndFilters(t *testing.T) {
	ctx := context.Background()
	s := openEmbedded(t, t.TempDir())

	if err := s.Upsert(ctx, testItems()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Query near [1,0,0]: best is o/a chunk 0.
	matches, err := s.Search(ctx, "", []float32{1, 0, 0}, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(matches) != 2 {
		t.Fatalf("got %d matches want 2", len(matches))
	}

	if matches[0].Payload.Repo != "o/a" || matches[0].Payload.Chunk != "alpha" {
		t.Fatalf("top match = %+v", matches[0].Payload)
	}

	if matches[0].Score < matches[1].Score {
		t.Fatal("matches not sorted by score desc")
	}

	// Repo filter restricts results.
	matches, err = s.Search(ctx, "o/b", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("Search filtered: %v", err)
	}

	if len(matches) != 1 || matches[0].Payload.Repo != "o/b" {
		t.Fatalf("filtered matches = %+v", matches)
	}
}

func TestEmbeddedUpsertOverwritesByID(t *testing.T) {
	ctx := context.Background()
	s := openEmbedded(t, t.TempDir())

	if err := s.Upsert(ctx, testItems()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Re-upsert the same ID with a new vector + chunk text.
	updated := []Item{{
		ID:      "o/a/doc.md#0",
		Vector:  []float32{0, 0, 1},
		Payload: Payload{Repo: "o/a", DocPath: "doc.md", Title: "A", Chunk: "alpha v2"},
	}}
	if err := s.Upsert(ctx, updated); err != nil {
		t.Fatalf("Upsert overwrite: %v", err)
	}

	matches, err := s.Search(ctx, "o/a", []float32{0, 0, 1}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(matches) != 1 || matches[0].Payload.Chunk != "alpha v2" {
		t.Fatalf("expected updated chunk, got %+v", matches)
	}
}

func TestEmbeddedPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s, err := newEmbedded(dir)
	if err != nil {
		t.Fatalf("newEmbedded: %v", err)
	}

	if err := s.Upsert(ctx, testItems()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openEmbedded(t, dir)

	matches, err := s2.Search(ctx, "", []float32{0, 1, 0}, 1)
	if err != nil {
		t.Fatalf("Search after reopen: %v", err)
	}

	if len(matches) != 1 || matches[0].Payload.Repo != "o/b" {
		t.Fatalf("matches after reopen = %+v", matches)
	}
}

func TestEmbeddedSharedHandleAcrossSwap(t *testing.T) {
	// Manager.Configure opens the new store before closing the old one; both
	// must share the same underlying DB (Badger allows one open per dir).
	ctx := context.Background()
	dir := t.TempDir()

	s1 := openEmbedded(t, dir)

	if err := s1.Upsert(ctx, testItems()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	s2 := openEmbedded(t, dir) // second open while first is still alive

	if err := s1.Close(); err != nil {
		t.Fatalf("close first handle: %v", err)
	}

	matches, err := s2.Search(ctx, "", []float32{1, 0, 0}, 1)
	if err != nil {
		t.Fatalf("Search on surviving handle: %v", err)
	}

	if len(matches) != 1 {
		t.Fatalf("got %d matches want 1", len(matches))
	}
}

func TestEmbeddedDeleteRepo(t *testing.T) {
	ctx := context.Background()
	s := openEmbedded(t, t.TempDir())

	if err := s.Upsert(ctx, testItems()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.DeleteRepo(ctx, "o/a"); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}

	matches, err := s.Search(ctx, "", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, m := range matches {
		if m.Payload.Repo == "o/a" {
			t.Fatalf("deleted repo still in results: %+v", m.Payload)
		}
	}
}

func TestEmbeddedDimChangeWipesAndRebuilds(t *testing.T) {
	// A new embedding model changes the dimension; the derived index must be
	// wiped and the new vectors accepted.
	ctx := context.Background()
	s := openEmbedded(t, t.TempDir())

	if err := s.Upsert(ctx, testItems()); err != nil { // dim=3
		t.Fatalf("Upsert dim=3: %v", err)
	}

	four := []Item{{
		ID:      "o/a/doc.md#0",
		Vector:  []float32{1, 0, 0, 0},
		Payload: Payload{Repo: "o/a", DocPath: "doc.md", Title: "A", Chunk: "alpha"},
	}}
	if err := s.Upsert(ctx, four); err != nil { // dim=4 -> wipe + retry
		t.Fatalf("Upsert dim=4: %v", err)
	}

	matches, err := s.Search(ctx, "", []float32{1, 0, 0, 0}, 10)
	if err != nil {
		t.Fatalf("Search dim=4: %v", err)
	}

	if len(matches) != 1 || matches[0].Payload.Repo != "o/a" {
		t.Fatalf("matches after dim change = %+v", matches)
	}
}

func TestEmbeddedConcurrentDimChangeWipesOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	s1 := openEmbedded(t, dir)
	s2 := openEmbedded(t, dir)

	if err := s1.Upsert(ctx, testItems()); err != nil { // establish dim=3
		t.Fatalf("Upsert dim=3: %v", err)
	}

	batches := [][]Item{
		{{ID: "o/a/new.go#0", Vector: []float32{1, 0, 0, 0}, Payload: Payload{Repo: "o/a", DocPath: "new.go", Chunk: "one"}}},
		{{ID: "o/b/new.go#0", Vector: []float32{0, 1, 0, 0}, Payload: Payload{Repo: "o/b", DocPath: "new.go", Chunk: "two"}}},
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(batches))
	for i, batch := range batches {
		wg.Add(1)
		go func(store *embedded, items []Item) {
			defer wg.Done()
			errs <- store.Upsert(ctx, items)
		}([]*embedded{s1, s2}[i], batch)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent upsert: %v", err)
		}
	}

	matches, err := s1.Search(ctx, "", []float32{1, 0, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d records after concurrent migration, want 2", len(matches))
	}
}
