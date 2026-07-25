package storage_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/storage"
)

// TestRunShrinksOnOversizedTransaction is the unit-level contract: a write
// rejected as too big is retried smaller until it fits, and the discovered
// size is reused so the cost of finding it is paid once.
func TestRunShrinksOnOversizedTransaction(t *testing.T) {
	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}

	// Only batches of 12 or fewer commit, so the batcher must come down from
	// 100 through 50 and 25 to 12.
	const fits = 12

	var (
		sizes   []int
		written []int
	)

	b := storage.NewBatcher(100)
	err := storage.Run(b, items, func(batch []int) error {
		sizes = append(sizes, len(batch))
		if len(batch) > fits {
			return fmt.Errorf("bw: vector write: %w", badger.ErrTxnTooBig)
		}
		written = append(written, batch...)

		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(written) != len(items) {
		t.Fatalf("wrote %d items, want %d", len(written), len(items))
	}
	for i, v := range written {
		if v != items[i] {
			t.Fatalf("item %d = %d, want %d; order or contents changed", i, v, items[i])
		}
	}

	for _, n := range sizes {
		if n > fits && n != 100 && n != 50 && n != 25 {
			t.Errorf("attempted an unexpected batch size %d", n)
		}
	}

	// Once the size is known, no later batch may retry an oversized one: the
	// rejections must all happen while discovering the limit, at the start.
	lastReject := -1
	for i, n := range sizes {
		if n > fits {
			lastReject = i
		}
	}
	if lastReject > 3 {
		t.Errorf("still discovering the batch size at attempt %d; the size is not being remembered", lastReject)
	}
}

// TestRunPropagatesOtherErrors makes sure the retry loop is specific to the
// size limit and does not paper over real write failures.
func TestRunPropagatesOtherErrors(t *testing.T) {
	sentinel := errors.New("disk on fire")
	calls := 0

	err := storage.Run(storage.NewBatcher(10), make([]int, 100), func([]int) error {
		calls++

		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Errorf("Run returned %v, want the write error", err)
	}
	if calls != 1 {
		t.Errorf("write attempted %d times, want 1: a non-size error must not be retried", calls)
	}
}

// TestRunGivesUpOnASingleOversizedRecord covers the case no batching strategy
// can fix: one record that alone exceeds the transaction limit. Run must
// surface it rather than loop.
func TestRunGivesUpOnASingleOversizedRecord(t *testing.T) {
	calls := 0

	err := storage.Run(storage.NewBatcher(8), make([]int, 8), func([]int) error {
		calls++

		return badger.ErrTxnTooBig
	})

	if !errors.Is(err, badger.ErrTxnTooBig) {
		t.Errorf("Run returned %v, want ErrTxnTooBig", err)
	}
	// 8 -> 4 -> 2 -> 1, then give up: four attempts, not an infinite loop.
	if calls != 4 {
		t.Errorf("write attempted %d times, want 4 (8, 4, 2, 1 then give up)", calls)
	}
}

// wideRecord is deliberately fat: a 1536-dimension embedding plus a source
// chunk, which is what krabby actually stores.
type wideRecord struct {
	ID     string    `bw:"id,pk"`
	Repo   string    `bw:"repo,index"`
	Chunk  string    `bw:"chunk"`
	Vector []float32 `bw:"vector,vector(metric=cosine)"`
}

// TestBatchedWritesSurviveASmallMemtable is the regression test for the
// failure this batcher exists for. Badger derives its per-transaction limit
// from the memtable size (15% of it), so tuning the memtable down for memory
// reasons also shrinks how much can be written at once — and a batch size
// chosen against the old limit starts failing with ErrTxnTooBig. The write
// path must adapt instead of breaking.
func TestBatchedWritesSurviveASmallMemtable(t *testing.T) {
	budget := memlimit.New(4<<30, memlimit.DefaultRatio)

	db, err := storage.OpenTuned(filepath.Join(t.TempDir(), "vectors"), budget)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	bucket, err := bw.RegisterBucket[wideRecord](db, "chunks")
	if err != nil {
		t.Fatalf("register bucket: %v", err)
	}

	const (
		records = 512
		dim     = 1536
		chunkSz = 3000
	)

	chunk := string(make([]byte, chunkSz))
	items := make([]*wideRecord, 0, records)
	for i := range records {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = float32(i+j) * 0.001
		}
		items = append(items, &wideRecord{
			ID:     fmt.Sprintf("repo/doc-%04d", i),
			Repo:   "repo",
			Chunk:  chunk,
			Vector: vec,
		})
	}

	// One unbatched transaction of all 512 must be rejected, or the test is
	// not reproducing the condition the batcher handles.
	if err := bucket.InsertMany(t.Context(), items); !errors.Is(err, badger.ErrTxnTooBig) {
		t.Fatalf("inserting %d wide records in one transaction returned %v, "+
			"want ErrTxnTooBig; the memtable is no longer small enough to exercise this", records, err)
	}

	batcher := storage.NewBatcher(records)
	if err := storage.Run(batcher, items, func(batch []*wideRecord) error {
		return bucket.InsertMany(t.Context(), batch)
	}); err != nil {
		t.Fatalf("batched insert: %v", err)
	}

	count, err := bucket.Count(t.Context(), nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != records {
		t.Errorf("stored %d records, want %d", count, records)
	}
}
