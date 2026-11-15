package repofs

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLines(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "line1\nline2\nline3\nline4\n")
	mustWrite(t, filepath.Join(dir, "noeol.txt"), "one\ntwo")

	for name, tt := range map[string]struct {
		file       string
		start, end int
		want       string
		gotEnd     int
	}{
		"a middle range":              {"a.go", 2, 3, "line2\nline3\n", 3},
		"one line":                    {"a.go", 4, 4, "line4\n", 4},
		"past the end is clipped":     {"a.go", 3, 99, "line3\nline4\n", 4},
		"no trailing newline":         {"noeol.txt", 1, 2, "one\ntwo", 2},
		"last line without a newline": {"noeol.txt", 2, 2, "two", 2},
	} {
		got, err := ReadLines(dir, tt.file, tt.start, tt.end)
		if err != nil {
			t.Errorf("%s: %v", name, err)

			continue
		}
		if got.Content != tt.want || got.Start != tt.start || got.End != tt.gotEnd || got.Truncated {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}

func TestReadLinesRejects(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "line1\nline2\n")

	for name, tt := range map[string]struct {
		file       string
		start, end int
		wantErr    string
	}{
		"zero start":         {"a.go", 0, 2, "invalid line range"},
		"backwards":          {"a.go", 2, 1, "invalid line range"},
		"start past the end": {"a.go", 3, 4, "has 2 lines"},
		"escaping the repo":  {"../a.go", 1, 1, "escapes repository root"},
		"a missing file":     {"nope.go", 1, 1, "nope.go"},
	} {
		if _, err := ReadLines(dir, tt.file, tt.start, tt.end); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tt.wantErr)
		}
	}
}

// A file larger than the read limit must not hand back a half line as if it
// were whole, and must say when the range was cut.
func TestReadLinesPastTheByteLimit(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("x", 99) + "\n" // 100 bytes
	n := MaxFileBytes/100 + 10
	mustWrite(t, filepath.Join(dir, "big.txt"), strings.Repeat(line, n))

	inside := MaxFileBytes / 100
	got, err := ReadLines(dir, "big.txt", inside-1, inside+5)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.End >= inside+5 {
		t.Errorf("range crossing the byte limit = %+v, want it cut and marked truncated", got)
	}
	for _, l := range strings.SplitAfter(got.Content, "\n") {
		if l != "" && l != line {
			t.Fatalf("returned a partial line of %d bytes", len(l))
		}
	}

	if _, err := ReadLines(dir, "big.txt", n, n); err == nil || !strings.Contains(err.Error(), "read_file with an offset") {
		t.Errorf("a line beyond the byte limit: err = %v, want a pointer to read_file", err)
	}
}
