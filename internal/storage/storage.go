// Package storage opens krabby's embedded databases with a bounded memory
// footprint.
//
// krabby runs three Badger databases (state, docs vectors, code vectors), and
// Badger's stock sizing is tuned for dedicated hosts rather than a few-gigabyte
// container. Every knob below is therefore derived from one process-wide
// budget (see internal/memlimit) and applied identically to all three.
package storage

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/memlimit"
)

// memFileExt is Badger's write-ahead-log extension for an unflushed memtable.
// Badger deletes these on a clean shutdown, so their presence means the last
// run was killed.
const memFileExt = ".mem"

// Open creates/opens the bw state database under the process memory budget.
func Open(path string) (*bw.DB, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir; %w", err)
	}

	db, err := OpenTuned(path, memlimit.Current())
	if err != nil {
		return nil, fmt.Errorf("open state db %s; %w", path, err)
	}

	return db, nil
}

// OpenTuned opens a bw database under the given memory budget.
//
// Badger allocates a full skiplist arena (MemTableSize plus ~30% headroom) for
// every .mem file it finds at startup, so shrinking MemTableSize is the single
// most effective way to make recovery from an OOM kill affordable. It is not
// free, though: the arena must be large enough to replay the log it was
// written with, and a log written under the old 64 MiB sizing can overflow a
// smaller arena and panic inside badger.Open. Leftover logs are therefore
// drained first, with the original sizing, and only then is the database
// reopened with the tuned one. That drain runs at most once per database,
// because a clean close deletes the logs.
func OpenTuned(path string, budget memlimit.Budget, opts ...bw.Option) (*bw.DB, error) {
	if n := countMemFiles(path); n > 0 && budget.MemTable < legacyMemTableSize() {
		slog.Warn("draining write-ahead logs left by an unclean shutdown before applying the tuned memtable size",
			"db", path, "logs", n, "legacy_memtable", memlimit.Bytes(legacyMemTableSize()),
			"tuned_memtable", memlimit.Bytes(budget.MemTable))

		if err := drainMemFiles(path, budget, opts...); err != nil {
			return nil, err
		}
	}

	return bw.Open(path, append(TuneOptions(budget), opts...)...)
}

// TuneOptions returns the bw options that bound a single database's memory use.
// Exported so callers that open a bw database directly (and tests that need to
// reproduce an untuned open) share one definition.
func TuneOptions(budget memlimit.Budget) []bw.Option {
	return []bw.Option{
		bw.WithLogger(nil),
		// The decoded-vector cache is the one bound GOMEMLIMIT cannot enforce:
		// its contents are live, so the collector can only thrash against
		// them. Its cost is set by the embedding model's width — the same
		// number of vectors is 75 MB at 96 dimensions and 1.2 GB at 1536 —
		// which is why the budget is bytes and not a vector count.
		bw.WithVectorCacheBytes(budget.VectorCache),
		bw.WithBadgerTune(func(bo *badger.Options) {
			applyTuning(bo, budget)
		}),
	}
}

// applyTuning sets the Badger knobs that drive resident memory.
func applyTuning(bo *badger.Options, budget memlimit.Budget) {
	// Memtables dominate startup cost: one arena per in-flight or recovered
	// write-ahead log. Two is enough to keep writes flowing while one flushes.
	bo.MemTableSize = budget.MemTable
	bo.NumMemtables = 2
	// Level sizes are derived from the memtable, so keep them in step rather
	// than leaving Badger's 2 MiB default against a much smaller memtable.
	bo.BaseTableSize = max(budget.MemTable/4, 2<<20)

	bo.BlockCacheSize = budget.BlockCache
	// Zero means "keep every table index resident forever". Bounding it caps
	// what a large vector store can pin as it grows.
	bo.IndexCacheSize = budget.IndexCache

	// Compaction runs concurrently and each compactor buffers tables in
	// memory; four is generous for an embedded workload.
	bo.NumCompactors = 2
	bo.NumLevelZeroTables = 2
	bo.NumLevelZeroTablesStall = 4

	// Detach memory pressure from the block cache by keeping value log files
	// small enough that the mmap of a single file cannot dwarf the budget.
	bo.ValueLogFileSize = min(bo.ValueLogFileSize, 64<<20)
}

// legacyMemTableSize is the memtable size krabby used before tuning, i.e.
// Badger's own default. Write-ahead logs on disk were written against it.
func legacyMemTableSize() int64 {
	return badger.DefaultOptions("").MemTableSize
}

// drainMemFiles opens the database with the legacy memtable sizing and closes
// it again. The close flushes every recovered memtable into L0 and deletes its
// write-ahead log, so the next open has nothing left to replay.
func drainMemFiles(path string, budget memlimit.Budget, opts ...bw.Option) error {
	legacy := budget
	legacy.MemTable = legacyMemTableSize()

	db, err := bw.Open(path, append(TuneOptions(legacy), opts...)...)
	if err != nil {
		return fmt.Errorf("drain write-ahead logs in %s; %w", path, err)
	}

	if err := db.Close(); err != nil {
		return fmt.Errorf("close after draining write-ahead logs in %s; %w", path, err)
	}

	return nil
}

// countMemFiles reports how many unflushed write-ahead logs the database left
// behind. A missing directory counts as none.
func countMemFiles(path string) int {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}

	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), memFileExt) {
			n++
		}
	}

	return n
}

// DirSize returns the on-disk size of a database directory, for logging how
// much data the tuned caches are working against. It reports 0 on any error.
func DirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort accounting
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}

		return nil
	})

	return total
}
