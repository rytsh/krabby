// Package memlimit derives krabby's process memory budget and turns it into
// the concrete knobs that actually bound RSS: Go's soft heap limit
// (GOMEMLIMIT), the per-database Badger cache/memtable sizes and the parsed
// graph cache.
//
// Without a budget krabby is trivially OOM-killed in a container: Go's garbage
// collector targets roughly twice the live heap, Badger allocates a full
// memtable arena per recovered write-ahead log, and every embedded database
// keeps its own block cache. Those defaults are individually reasonable and
// collectively larger than a typical 4 GiB container limit, so they are all
// derived from one number here instead of being tuned in four places.
package memlimit

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
)

// Defaults for the budget split. They are deliberately conservative: krabby
// also shells out to the graphify CLI (a Python process) which lives in the
// same cgroup, so the Go heap must not be allowed to claim the whole limit.
const (
	// DefaultRatio is the fraction of the detected limit handed to the Go
	// runtime as GOMEMLIMIT. The remainder covers the graphify subprocess,
	// git, mmap'd Badger tables and runtime overhead outside the heap.
	DefaultRatio = 0.75

	// minLimit guards against a nonsensically small detected limit (or a
	// misconfigured one) producing caches too small for Badger to open.
	minLimit = 256 << 20 // 256 MiB
)

// Budget is the resolved memory plan for the process.
type Budget struct {
	// Total is the memory limit krabby believes it has, in bytes.
	Total int64
	// Source describes where Total came from, for logging.
	Source string
	// GoLimit is the soft heap limit applied via debug.SetMemoryLimit. Zero
	// means the limit was left untouched (already set through the
	// GOMEMLIMIT environment variable).
	GoLimit int64

	// BlockCache is the per-database Badger block cache size.
	BlockCache int64
	// IndexCache is the per-database Badger table-index cache size.
	IndexCache int64
	// MemTable is the per-database Badger memtable size. This is the single
	// most important knob on restart: Badger allocates an arena of roughly
	// MemTable + 2*(15% of MemTable) for every .mem file left behind by an
	// unclean shutdown, so a large value multiplies the cost of recovering
	// from a previous OOM kill.
	MemTable int64
	// VectorCache is the per-vector-index budget for decoded embeddings held
	// in memory during HNSW traversal. It is sized in bytes rather than
	// vectors on purpose: the same count costs an order of magnitude more at
	// 1536 dimensions than at 96, and this cache is live memory that no amount
	// of garbage collection can reclaim.
	VectorCache int64
	// GraphCache bounds the parsed graphify graphs held by the query engine.
	GraphCache int64
}

// current holds the process-wide budget. The memory limit is a property of the
// process, like GOMAXPROCS, and is consulted by every embedded database as it
// opens; threading it through each constructor would add a parameter to call
// sites (including tests) that have no opinion about it. Callers that do want
// an explicit budget can still pass one to the *Tuned constructors.
var current atomic.Pointer[Budget]

// Set installs the budget resolved at startup.
func Set(b Budget) { current.Store(&b) }

// Current returns the process budget, auto-detecting one on first use so
// tests and library callers get the same bounded defaults as the server.
func Current() Budget {
	if b := current.Load(); b != nil {
		return *b
	}

	b := New(0, DefaultRatio)
	current.CompareAndSwap(nil, &b)

	return *current.Load()
}

// New resolves the budget. limit is the operator-configured total in bytes; 0
// means auto-detect from the cgroup (container) limit, falling back to total
// system memory. ratio is the fraction of the total handed to the Go heap;
// non-positive values use DefaultRatio.
func New(limit int64, ratio float64) Budget {
	b := Budget{Total: limit, Source: "config"}
	if b.Total <= 0 {
		b.Total, b.Source = detect()
	}
	if b.Total < minLimit {
		b.Total, b.Source = minLimit, b.Source+" (raised to minimum)"
	}

	if ratio <= 0 || ratio > 1 {
		ratio = DefaultRatio
	}
	b.GoLimit = int64(float64(b.Total) * ratio)

	// Badger opens three databases (state, docs vectors, code vectors), so
	// every per-database figure below is paid three times. The vector cache is
	// paid twice: only the docs and code stores hold embeddings.
	b.BlockCache = clamp(b.Total/64, 16<<20, 64<<20)
	b.IndexCache = clamp(b.Total/128, 8<<20, 32<<20)
	b.MemTable = clamp(b.Total/256, 8<<20, 32<<20)
	b.VectorCache = clamp(b.Total/64, 16<<20, 96<<20)
	b.GraphCache = clamp(b.Total/16, 64<<20, 512<<20)

	return b
}

// Apply installs the Go soft heap limit. An explicit GOMEMLIMIT in the
// environment wins: the operator has already made the decision and silently
// overriding it would be surprising. Apply reports the limit it left in place.
func (b *Budget) Apply() {
	// SetMemoryLimit(-1) reads the current value without changing it. The
	// runtime default is math.MaxInt64, so anything smaller means GOMEMLIMIT
	// was set in the environment.
	if cur := debug.SetMemoryLimit(-1); cur != math.MaxInt64 {
		b.GoLimit = 0
		b.Source += fmt.Sprintf(" (GOMEMLIMIT already set to %s)", Bytes(cur))

		return
	}

	debug.SetMemoryLimit(b.GoLimit)
}

// detect resolves the memory limit from the cgroup the process belongs to,
// falling back to total system memory.
func detect() (int64, string) {
	// cgroup v2. In a container /sys/fs/cgroup is the namespaced root, so the
	// limit is readable directly.
	if v, ok := readCgroupValue("/sys/fs/cgroup/memory.max"); ok {
		return v, "cgroup v2"
	}
	// cgroup v1.
	if v, ok := readCgroupValue("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok {
		return v, "cgroup v1"
	}
	if v, ok := memTotal(); ok {
		return v, "system memory"
	}

	return minLimit, "fallback"
}

// readCgroupValue reads a cgroup memory limit file. It reports false for a
// missing file, the literal "max", and the sentinel values cgroup v1 uses for
// "unlimited" (a limit close to the int64 ceiling), so callers fall through to
// the next source.
func readCgroupValue(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	s := strings.TrimSpace(string(raw))
	if s == "" || s == "max" {
		return 0, false
	}

	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}

	// cgroup v1 reports "unlimited" as PAGE_COUNTER_MAX scaled by the page
	// size, which lands just under math.MaxInt64. Anything at or above a
	// petabyte is not a real container limit.
	if v >= 1<<50 {
		return 0, false
	}

	return v, true
}

// memTotal reads MemTotal from /proc/meminfo (kibibytes).
func memTotal() (int64, bool) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}

	for line := range strings.SplitSeq(string(raw), "\n") {
		after, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}

		fields := strings.Fields(after)
		if len(fields) == 0 {
			return 0, false
		}

		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb <= 0 {
			return 0, false
		}

		return kb << 10, true
	}

	return 0, false
}

func clamp(v, lo, hi int64) int64 {
	return min(max(v, lo), hi)
}

// Bytes formats a byte count for logs.
func Bytes(v int64) string {
	const unit = 1024
	if v < unit {
		return strconv.FormatInt(v, 10) + "B"
	}

	div, exp := int64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f%ciB", float64(v)/float64(div), "KMGTPE"[exp])
}
