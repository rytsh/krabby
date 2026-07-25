package memlimit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewDerivesBudgetFromLimit(t *testing.T) {
	// The 4 GiB container that motivated this package.
	b := New(4<<30, DefaultRatio)

	if b.GoLimit != 3<<30 {
		t.Errorf("GoLimit = %s, want 3.0GiB", Bytes(b.GoLimit))
	}

	// Three databases pay each per-database figure, so their combined caches
	// plus the graph cache must leave the Go heap real room to work in.
	perDB := b.BlockCache + b.IndexCache + b.MemTable*2
	if total := perDB*3 + b.GraphCache; total > b.GoLimit/2 {
		t.Errorf("caches total %s, more than half of the %s heap limit",
			Bytes(total), Bytes(b.GoLimit))
	}

	// The memtable is the knob that decides how expensive recovering from an
	// unclean shutdown is, so it must stay far below Badger's own 64 MiB.
	if b.MemTable > 32<<20 {
		t.Errorf("MemTable = %s, want at most 32.0MiB", Bytes(b.MemTable))
	}
}

func TestNewScalesWithTheLimit(t *testing.T) {
	small := New(512<<20, DefaultRatio)
	large := New(32<<30, DefaultRatio)

	if small.BlockCache >= large.BlockCache {
		t.Errorf("block cache did not grow with the limit: %s vs %s",
			Bytes(small.BlockCache), Bytes(large.BlockCache))
	}
	if small.GraphCache >= large.GraphCache {
		t.Errorf("graph cache did not grow with the limit: %s vs %s",
			Bytes(small.GraphCache), Bytes(large.GraphCache))
	}

	// Both ends stay clamped: a huge host must not hand Badger an unbounded
	// cache, and a tiny one must still leave it enough to open.
	if large.BlockCache > 64<<20 {
		t.Errorf("block cache %s exceeds its cap", Bytes(large.BlockCache))
	}
	if small.MemTable < 8<<20 {
		t.Errorf("memtable %s is below its floor", Bytes(small.MemTable))
	}
}

func TestNewRejectsNonsenseInput(t *testing.T) {
	tests := []struct {
		name  string
		limit int64
		ratio float64
	}{
		{"negative ratio", 4 << 30, -1},
		{"ratio above one", 4 << 30, 5},
		{"zero ratio", 4 << 30, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New(tt.limit, tt.ratio)
			if want := int64(float64(tt.limit) * DefaultRatio); b.GoLimit != want {
				t.Errorf("GoLimit = %s, want the default ratio applied (%s)",
					Bytes(b.GoLimit), Bytes(want))
			}
		})
	}
}

func TestNewRaisesAnUnusablySmallLimit(t *testing.T) {
	b := New(1<<20, DefaultRatio)

	if b.Total != minLimit {
		t.Errorf("Total = %s, want it raised to %s", Bytes(b.Total), Bytes(minLimit))
	}
	if b.MemTable <= 0 || b.BlockCache <= 0 {
		t.Errorf("budget has non-positive cache sizes: %+v", b)
	}
}

func TestReadCgroupValue(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		want    int64
		wantOK  bool
	}{
		{"container limit", "4294967296\n", 4 << 30, true},
		{"cgroup v2 unlimited", "max\n", 0, false},
		{"cgroup v1 unlimited", "9223372036854771712\n", 0, false},
		{"empty", "", 0, false},
		{"garbage", "not-a-number", 0, false},
		{"zero", "0", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}

			got, ok := readCgroupValue(path)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("readCgroupValue = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}

	if _, ok := readCgroupValue(filepath.Join(dir, "missing")); ok {
		t.Error("a missing file reported a limit")
	}
}

func TestBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{512, "512B"},
		{4 << 30, "4.0GiB"},
		{16 << 20, "16.0MiB"},
		{1536 << 20, "1.5GiB"},
	}

	for _, tt := range tests {
		if got := Bytes(tt.in); got != tt.want {
			t.Errorf("Bytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCurrentAutoDetects(t *testing.T) {
	// Current must never hand out a zero budget, because every database open
	// sizes its caches from it.
	b := Current()

	if b.Total <= 0 || b.MemTable <= 0 || b.BlockCache <= 0 || b.GraphCache <= 0 {
		t.Errorf("auto-detected budget is unusable: %+v", b)
	}
}
