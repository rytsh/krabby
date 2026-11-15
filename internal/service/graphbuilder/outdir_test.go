package graphbuilder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A repository can commit .bag as a symlink. The build must not write through
// it: the graph has to land inside the clone, and whatever the link pointed at
// must stay untouched.
func TestUpdateDoesNotWriteThroughACommittedSymlink(t *testing.T) {
	outside := t.TempDir()
	clone := t.TempDir()
	if err := os.WriteFile(filepath.Join(clone, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(clone, ".bag")); err != nil {
		t.Fatal(err)
	}

	if err := New(time.Minute, nil).Update(context.Background(), clone, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the build wrote %d entries through the symlink, e.g. %s", len(entries), entries[0].Name())
	}

	info, err := os.Lstat(filepath.Join(clone, ".bag"))
	if err != nil || !info.IsDir() {
		t.Fatalf(".bag is not a real directory after the build: %v %v", info, err)
	}
	if _, err := os.Stat(GraphPath(clone)); err != nil {
		t.Fatalf("graph not written inside the clone: %v", err)
	}
}

// The legacy ignore-file cleanup rewrites the file; through a symlink that
// would be a write wherever the link points.
func TestRemoveLegacyManagedIgnoreLeavesASymlinkAlone(t *testing.T) {
	target := filepath.Join(t.TempDir(), "victim")
	content := "keep me\n" + managedBegin + "\nx\n" + managedEnd + "\n"
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	clone := t.TempDir()
	if err := os.Symlink(target, filepath.Join(clone, legacyIgnoreFileName)); err != nil {
		t.Fatal(err)
	}

	if changed, err := RemoveLegacyManagedIgnore(clone); err != nil || changed {
		t.Fatalf("RemoveLegacyManagedIgnore = %v, %v; want it to leave a symlink alone", changed, err)
	}
	if got, _ := os.ReadFile(target); string(got) != content {
		t.Fatalf("the link's target was rewritten: %q", got)
	}
}

// A begin marker without its end must not swallow the user's lines after it.
func TestStripManagedBlockKeepsAnUnterminatedFile(t *testing.T) {
	in := "user-a\n" + managedBegin + "\nmanaged\nuser-b\n"
	if got := stripManagedBlock(in); got != in {
		t.Fatalf("stripManagedBlock changed an unterminated file:\n%q", got)
	}
}

// build_timeout 0 means no limit, not a deadline that has already passed.
func TestZeroTimeoutIsNoLimit(t *testing.T) {
	ctx, cancel := New(0, nil).timeout(context.Background())
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("a zero timeout produced an expired context: %v", err)
	}
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("a zero timeout set a deadline")
	}
}

// MergeExclude never hands back one of its inputs: a caller that appends to
// the result must not grow the install-wide defaults.
func TestMergeExcludeDoesNotAlias(t *testing.T) {
	global := make([]string, 1, 8)
	global[0] = "vendor/"
	out := MergeExclude(global, nil)
	out = append(out, "extra/")
	if strings.Join(global[:cap(global)][:2], ",") == "vendor/,extra/" {
		t.Fatal("appending to the result wrote into the global slice")
	}
	_ = out
}
