package vectorstore

import (
	"context"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/storage"
)

// seedV2Store writes records in the pre-v3 shape: no Kind, and the embedding
// stored inline in the record body. It closes the database so the caller can
// reopen it through the normal constructor and exercise the migration.
func seedV2Store(t *testing.T, dir string, records []*chunkRecordV2) {
	t.Helper()

	db, err := storage.OpenTuned(dir, memlimit.Current())
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}

	bucket, err := bw.RegisterBucket[chunkRecordV2](db, bucketName, bw.WithVersion[chunkRecordV2](2))
	if err != nil {
		_ = db.Close()
		t.Fatalf("register v2 bucket: %v", err)
	}

	ctx := context.Background()
	for _, rec := range records {
		if err := bucket.Insert(ctx, rec); err != nil {
			_ = db.Close()
			t.Fatalf("insert %s: %v", rec.ID, err)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close v2 db: %v", err)
	}
}

// TestMigrateChunksV2ToV3 is the guard on the riskiest part of the upgrade.
//
// The v3 shape moved the scope discriminator into an indexed field and stopped
// storing the embedding inside the record. Both are invisible in the record
// the caller sees, so a broken migration would not fail loudly — it would
// return fewer results, forever, until someone happened to re-index. This
// checks the three properties that would actually break:
//
//   - the embeddings survive, so nothing has to be paid for again;
//   - Kind is backfilled, so scope searches still match;
//   - search still ranks correctly afterwards.
func TestMigrateChunksV2ToV3(t *testing.T) {
	dir := t.TempDir()
	updated := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	seedV2Store(t, dir, []*chunkRecordV2{
		{
			ID:        "o/a/doc.md#0",
			Repo:      "o/a",
			DocPath:   "doc.md",
			Title:     "A",
			Chunk:     "alpha",
			UpdatedAt: updated,
			Vector:    []float32{1, 0, 0},
		},
		{
			ID:      "o/b/doc.md#0",
			Repo:    "o/b",
			DocPath: "doc.md",
			Title:   "B",
			Chunk:   "beta",
			Vector:  []float32{0, 1, 0},
		},
		{
			ID:      "web:wiki/page.md#0",
			Repo:    "web:wiki",
			DocPath: "page.md",
			Title:   "W",
			Chunk:   "wiki",
			Vector:  []float32{0, 0, 1},
		},
	})

	// Reopening through the normal constructor runs the migration.
	s := openEmbedded(t, dir)
	ctx := context.Background()

	t.Run("embeddings survive the migration", func(t *testing.T) {
		// Querying with a seeded vector must return its own record first,
		// which is only possible if the vector was carried across.
		for _, tc := range []struct {
			vec  []float32
			want string
		}{
			{[]float32{1, 0, 0}, "o/a"},
			{[]float32{0, 1, 0}, "o/b"},
			{[]float32{0, 0, 1}, "web:wiki"},
		} {
			matches, err := s.Search(ctx, Filter{}, tc.vec, 1)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(matches) != 1 {
				t.Fatalf("got %d matches, want 1", len(matches))
			}
			if matches[0].Payload.Repo != tc.want {
				t.Fatalf("got repo %q, want %q", matches[0].Payload.Repo, tc.want)
			}
		}
	})

	t.Run("payload fields survive", func(t *testing.T) {
		matches, err := s.Search(ctx, FilterKey("o/a"), []float32{1, 0, 0}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 1 {
			t.Fatalf("got %d matches, want 1", len(matches))
		}

		got := matches[0].Payload
		if got.DocPath != "doc.md" || got.Title != "A" || got.Chunk != "alpha" {
			t.Fatalf("payload = %+v", got)
		}
		if !got.UpdatedAt.Equal(updated) {
			t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, updated)
		}
	})

	t.Run("kind is backfilled so scope search matches", func(t *testing.T) {
		web, err := s.Search(ctx, Filter{Kind: KindWeb}, []float32{0, 0, 1}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(web) != 1 || web[0].Payload.Repo != "web:wiki" {
			t.Fatalf("web scope = %+v", web)
		}

		repos, err := s.Search(ctx, Filter{Kind: KindRepo}, []float32{1, 0, 0}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(repos) != 2 {
			t.Fatalf("repo scope returned %d, want the 2 repo-backed chunks", len(repos))
		}
		for _, m := range repos {
			if m.Payload.Repo == "web:wiki" {
				t.Fatalf("web key leaked into the repo scope: %+v", m.Payload)
			}
		}
	})

	t.Run("migration is not re-run on the next open", func(t *testing.T) {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		again := openEmbedded(t, dir)

		matches, err := again.Search(ctx, Filter{Kind: KindRepo}, []float32{1, 0, 0}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 2 {
			t.Fatalf("after reopen, repo scope returned %d, want 2", len(matches))
		}
	})
}

// TestFreshStoreRegistersAtCurrentVersion covers the other path through the
// same code: a store created from scratch has nothing to migrate and must
// simply work.
func TestFreshStoreRegistersAtCurrentVersion(t *testing.T) {
	ctx := context.Background()
	s := openEmbedded(t, t.TempDir())

	if err := s.Upsert(ctx, testItems()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	matches, err := s.Search(ctx, Filter{Kind: KindRepo}, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 3 {
		t.Fatalf("got %d matches, want 3", len(matches))
	}
}
