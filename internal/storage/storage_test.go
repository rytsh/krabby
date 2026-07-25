package storage_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/storage"
)

// legacyMemTable is Badger's own memtable size, which krabby used before the
// budget-derived tuning. Write-ahead logs left on disk by an older build were
// written against it.
const legacyMemTable = 64 << 20

// testBudget is the budget for a 4 GiB container, the size that motivated this
// work.
func testBudget() memlimit.Budget { return memlimit.New(4<<30, memlimit.DefaultRatio) }

// TestOpenTunedBoundsStartupHeap pins the property the tuning exists for: the
// three databases krabby opens must cost a bounded, small amount of heap before
// a single record is written. Badger's stock sizing allocates a full memtable
// arena (64 MiB plus ~30% headroom) per database, which is most of a small
// container's budget spent on empty stores.
func TestOpenTunedBoundsStartupHeap(t *testing.T) {
	budget := testBudget()

	before := heapAlloc()

	for _, name := range []string{"state", "docs-vectors", "code-vectors"} {
		db, err := storage.OpenTuned(filepath.Join(t.TempDir(), name), budget)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		t.Cleanup(func() { _ = db.Close() })
	}

	used := heapAlloc() - before

	// Stock Badger costs ~89 MiB per database (~267 MiB for three). Under a
	// 4 GiB budget the tuned memtable is 16 MiB, so the three together must
	// stay well under half of the untuned figure.
	const limit = 120 << 20
	if used > limit {
		t.Fatalf("opening three databases allocated %s, want at most %s",
			memlimit.Bytes(used), memlimit.Bytes(limit))
	}

	t.Logf("three databases allocated %s", memlimit.Bytes(used))
}

// TestOpenTunedDrainsLogsFromKilledProcess covers recovery from the failure
// mode that motivated the tuning. An OOM kill is a SIGKILL: memtables never
// reach L0 and their write-ahead logs stay on disk, written against the old
// 64 MiB arena. Replaying such a log into a smaller arena panics inside
// badger.Open, so a naive shrink would turn one OOM kill into a permanently
// unstartable service. OpenTuned must drain the logs first and open cleanly.
func TestOpenTunedDrainsLogsFromKilledProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	killedWriter(t, dir)

	if n := countMemFiles(t, dir); n == 0 {
		t.Fatal("killed writer left no write-ahead logs; the test cannot exercise recovery")
	}

	budget := testBudget()
	if budget.MemTable >= legacyMemTable {
		t.Fatalf("tuned memtable %d is not smaller than the legacy %d", budget.MemTable, legacyMemTable)
	}

	db, err := storage.OpenTuned(dir, budget)
	if err != nil {
		t.Fatalf("reopen with tuned sizing after an unclean shutdown: %v", err)
	}

	bucket, err := bw.RegisterBucket[record](db, bucketName)
	if err != nil {
		t.Fatalf("register bucket: %v", err)
	}

	// The drained data must still be there: draining flushes the memtables to
	// L0, it does not discard them.
	for _, i := range []int{0, writtenRecords / 2, writtenRecords - 1} {
		got, gerr := bucket.Get(t.Context(), recordID(i))
		if gerr != nil {
			t.Fatalf("read record %d written before the kill: %v", i, gerr)
		}
		if len(got.Blob) != blobSize {
			t.Errorf("record %d blob length = %d, want %d", i, len(got.Blob), blobSize)
		}
	}

	// The reopen creates a fresh (empty) log of its own, so only a clean close
	// leaves the directory with none.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := countMemFiles(t, dir); n != 0 {
		t.Errorf("write-ahead logs remain after a clean close: %d", n)
	}
}

// TestTunedOpenWithoutDrainKillsTheProcess documents why OpenTuned drains
// first. Handing a log written against the legacy arena straight to a smaller
// one trips a Badger assertion, and that assertion is a log.Fatalf: the process
// exits and no caller can recover. A naive shrink would therefore turn a single
// OOM kill into a service that never starts again — hence the drain, and hence
// this must be verified in a child process.
//
// If Badger ever grows the arena on demand this test starts failing, and the
// drain can be deleted.
func TestTunedOpenWithoutDrainKillsTheProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	killedWriter(t, dir)

	if n := countMemFiles(t, dir); n == 0 {
		t.Fatal("killed writer left no write-ahead logs")
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestUntunedOpenHelper") //nolint:gosec // re-exec of the test binary
	cmd.Env = append(os.Environ(), untunedEnv+"="+dir)

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Error("opening a legacy write-ahead log with the tuned arena succeeded; " +
			"the drain in OpenTuned is no longer needed")

		return
	}

	t.Logf("child died as expected: %v\n%s", err, out)
}

// TestUntunedOpenHelper is not a test: it is the child spawned by
// TestTunedOpenWithoutDrainKillsTheProcess.
func TestUntunedOpenHelper(t *testing.T) {
	dir := os.Getenv(untunedEnv)
	if dir == "" {
		t.Skip("helper process; not run directly")
	}

	db, err := bw.Open(dir, storage.TuneOptions(testBudget())...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.Close()
}

// killedWriter runs this test binary as a child that fills a database under the
// legacy memtable sizing and is then SIGKILLed, leaving its write-ahead logs
// behind exactly as an OOM kill would.
func killedWriter(t *testing.T, dir string) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=TestWriterHelper") //nolint:gosec // re-exec of the test binary
	cmd.Env = append(os.Environ(), writerEnv+"="+dir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start writer: %v", err)
	}

	// The child signals readiness by creating the marker file, then blocks.
	waitForMarker(t, filepath.Join(dir, markerName))

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill writer: %v", err)
	}
	_ = cmd.Wait()
}

// TestWriterHelper is not a test: it is the child process spawned by
// killedWriter. It exits immediately unless that environment variable is set.
func TestWriterHelper(t *testing.T) {
	dir := os.Getenv(writerEnv)
	if dir == "" {
		t.Skip("helper process; not run directly")
	}

	budget := testBudget()
	budget.MemTable = legacyMemTable

	db, err := storage.OpenTuned(dir, budget)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	bucket, err := bw.RegisterBucket[record](db, bucketName)
	if err != nil {
		t.Fatalf("register bucket: %v", err)
	}

	// Write more than the tuned arena (16 MiB) but less than the legacy one so
	// the log genuinely requires the legacy sizing to replay.
	blob := string(make([]byte, blobSize))
	for i := range writtenRecords {
		if err := bucket.Insert(t.Context(), &record{ID: recordID(i), Blob: blob}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, markerName), nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	select {} // wait to be killed
}

const (
	writerEnv      = "KRABBY_TEST_WAL_WRITER_DIR"
	untunedEnv     = "KRABBY_TEST_UNTUNED_OPEN_DIR"
	markerName     = "writer.ready"
	bucketName     = "records"
	blobSize       = 64 << 10
	writtenRecords = 384 // ~24 MiB: over the tuned arena, under the legacy one
)

type record struct {
	ID   string `bw:"id,pk"`
	Blob string `bw:"blob"`
}

func recordID(i int) string { return "record-" + strconv.Itoa(i) }

// waitForMarker blocks until the child reports it has finished writing.
func waitForMarker(t *testing.T, path string) {
	t.Helper()

	for range 3000 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("writer did not become ready")
}

func countMemFiles(t *testing.T, dir string) int {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".mem" {
			n++
		}
	}

	return n
}

func heapAlloc() int64 {
	runtime.GC()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return int64(ms.HeapAlloc)
}
