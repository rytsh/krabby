package storage

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/dgraph-io/badger/v4"
)

// Batcher splits a slice of work into transactions that fit Badger's
// per-transaction limit.
//
// That limit is not a constant a caller can code against. Badger derives it
// from the memtable size (15% of it, and a matching entry count), and how much
// of it one record consumes depends on the record: an embedding is as wide as
// the model that produced it, and a full-text write expands into one key per
// distinct term in the document. A fixed batch size is therefore a guess about
// two things the code does not control, and the guess fails as ErrTxnTooBig
// once either moves — a smaller memtable, a wider embedding model, a more
// verbose document.
//
// Batcher starts at Max and halves on ErrTxnTooBig, keeping the reduced size
// for later calls. Records in one store are near-uniform, so the cost of
// discovering the right size is paid once rather than per batch. Badger
// discards a transaction that returns an error, so a rejected batch leaves
// nothing behind and retrying it smaller is safe.
type Batcher struct {
	mu  sync.Mutex
	max int
	cur int
}

// NewBatcher returns a batcher starting at max items per transaction.
func NewBatcher(max int) *Batcher {
	if max < 1 {
		max = 1
	}

	return &Batcher{max: max, cur: max}
}

// size reports the batch size to try next.
func (b *Batcher) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.cur
}

// shrink halves the batch size, reporting the new one and whether there was
// room left to shrink. A batch of one that still does not fit cannot be split
// further: the record itself is too large for a transaction.
func (b *Batcher) shrink() (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cur <= 1 {
		return 1, false
	}

	b.cur /= 2
	slog.Debug("write batch shrunk to fit the transaction limit",
		"from", b.max, "to", b.cur)

	return b.cur, true
}

// Run calls write for consecutive slices of items, shrinking the batch on
// ErrTxnTooBig and retrying until each slice commits. write must be
// idempotent with respect to a rejected batch, which Badger guarantees by
// discarding the transaction.
func Run[T any](b *Batcher, items []T, write func([]T) error) error {
	for start := 0; start < len(items); {
		n := min(b.size(), len(items)-start)

		for {
			err := write(items[start : start+n])
			if err == nil {
				start += n

				break
			}

			if !errors.Is(err, badger.ErrTxnTooBig) {
				return err
			}

			smaller, ok := b.shrink()
			if !ok {
				// One record on its own exceeds the transaction limit. No
				// batching strategy helps; the caller needs a bigger memtable
				// or a smaller record.
				return err
			}

			n = min(smaller, len(items)-start)
		}
	}

	return nil
}
