package vectorstore

import (
	"context"
	"testing"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/storage"
)

// The actual v3 on-disk shape, including external (non-inline) embeddings.
type chunkRecordBeforePathIndex struct {
	ID        string    `bw:"id,pk"`
	Repo      string    `bw:"repo,index"`
	Kind      string    `bw:"kind,index"`
	DocPath   string    `bw:"doc_path"`
	Title     string    `bw:"title"`
	Chunk     string    `bw:"chunk"`
	UpdatedAt time.Time `bw:"updated_at"`
	Symbol    string    `bw:"symbol"`
	StartLine int       `bw:"start_line"`
	EndLine   int       `bw:"end_line"`
	Vector    []float32 `bw:"vector,vector(metric=cosine,inline=false)"`
}

func TestPathIndexMigrationPreservesExternalVectors(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.OpenTuned(dir, memlimit.Current())
	if err != nil {
		t.Fatal(err)
	}
	old, err := bw.RegisterBucket[chunkRecordBeforePathIndex](db, bucketName, bw.WithVersion[chunkRecordBeforePathIndex](3))
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, rec := range []*chunkRecordBeforePathIndex{
		{ID: "opaque-a", Repo: "a", Kind: KindRepo, DocPath: "main.go", Chunk: "alpha", Vector: []float32{1, 0, 0}},
		{ID: "opaque-b", Repo: "a", Kind: KindRepo, DocPath: "other.go", Chunk: "beta", Vector: []float32{0, 1, 0}},
		{ID: "opaque-c", Repo: "b", Kind: KindRepo, DocPath: "main.go", Chunk: "gamma", Vector: []float32{0, 0, 1}},
	} {
		if err := old.Insert(ctx, rec); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openEmbedded(t, dir)
	matches, err := s.Search(ctx, Filter{}, []float32{1, 0, 0}, 10)
	if err != nil || len(matches) != 3 || matches[0].Payload.Chunk != "alpha" {
		t.Fatalf("migrated vectors=%+v err=%v", matches, err)
	}
	if err := s.DeletePaths(ctx, "a", []string{"main.go", "main.go", "missing.go"}); err != nil {
		t.Fatal(err)
	}
	matches, err = s.Search(ctx, Filter{}, []float32{0, 1, 0}, 10)
	if err != nil || len(matches) != 2 {
		t.Fatalf("after delete=%+v err=%v", matches, err)
	}
	for _, hit := range matches {
		if hit.Payload.Chunk == "alpha" {
			t.Fatal("path index missed the migrated record")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openEmbedded(t, dir)
	matches, err = s.Search(ctx, Filter{}, []float32{0, 0, 1}, 10)
	if err != nil || len(matches) != 2 || matches[0].Payload.Chunk != "gamma" {
		t.Fatalf("reopened vectors=%+v err=%v", matches, err)
	}
}
