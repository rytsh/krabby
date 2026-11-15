package graphbuilder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveLegacyManagedIgnore(t *testing.T) {
	clone := t.TempDir()
	path := filepath.Join(clone, legacyIgnoreFileName)
	content := "# my rules\nlocal/\n\n" + managedBegin + "\ntestdata/\n" + managedEnd + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := RemoveLegacyManagedIgnore(clone)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected legacy block to be removed")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "# my rules\nlocal/\n"; got != want {
		t.Fatalf("preserved content = %q, want %q", got, want)
	}
}

func TestRemoveLegacyManagedIgnoreDeletesGeneratedFile(t *testing.T) {
	clone := t.TempDir()
	path := filepath.Join(clone, legacyIgnoreFileName)
	content := managedBegin + "\ntestdata/\n" + managedEnd + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := RemoveLegacyManagedIgnore(clone)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected generated file to be removed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy file still exists: %v", err)
	}
}

func TestRemoveLegacyManagedIgnoreLeavesUserFileAlone(t *testing.T) {
	clone := t.TempDir()
	path := filepath.Join(clone, legacyIgnoreFileName)
	if err := os.WriteFile(path, []byte("local/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := RemoveLegacyManagedIgnore(clone)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("user-authored file was changed")
	}
}

// A repository's own patterns are unioned with the install-wide ones rather
// than replacing them: an install-wide rule is a policy, and a repo opting out
// of it is not a case worth supporting.
func TestPerRepoIgnorePatternsUnion(t *testing.T) {
	c := &Builder{exclude: []string{"global/"}}

	got := c.ignorePatterns([]string{"repo/"})
	if len(got) != 2 || got[0] != "global/" || got[1] != "repo/" {
		t.Fatalf("ignorePatterns = %v, want both lists", got)
	}

	// No repo patterns: the install-wide list is returned as-is.
	if got := c.ignorePatterns(nil); len(got) != 1 || got[0] != "global/" {
		t.Fatalf("ignorePatterns(nil) = %v", got)
	}

	// The install-wide slice is shared by every repo built in this process, so
	// unioning must not append into it.
	base := &Builder{exclude: make([]string, 1, 8)}
	base.exclude[0] = "global/"
	_ = base.ignorePatterns([]string{"repo/"})
	if len(base.exclude) != 1 || base.exclude[0] != "global/" {
		t.Fatalf("install-wide excludes mutated: %v", base.exclude)
	}
}

func writeGraphSource(t *testing.T, repo string) {
	t.Helper()

	for rel, src := range map[string]string{
		"a.py":     "def a():\n    return 1\n",
		"gen/b.py": "def b():\n    return 2\n",
	} {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The rebuild decision follows the rules a graph was built with, in both
// directions. A removed pattern is the case inspecting the graph cannot see:
// the files it should now contain are simply absent from it.
func TestGraphNeedsIgnoreRebuild(t *testing.T) {
	repo := t.TempDir()
	writeGraphSource(t, repo)

	built := New(time.Minute, []string{"global/"})
	if err := built.Update(context.Background(), repo, []string{"gen/"}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		global  []string
		extra   []string
		rebuild bool
	}{
		{name: "same rules", global: []string{"global/"}, extra: []string{"gen/"}},
		{name: "repo pattern added", global: []string{"global/"}, extra: []string{"gen/", "a.py"}, rebuild: true},
		{name: "repo pattern removed", global: []string{"global/"}, rebuild: true},
		{name: "install-wide pattern added", global: []string{"global/", "more/"}, extra: []string{"gen/"}, rebuild: true},
		{name: "install-wide pattern removed", extra: []string{"gen/"}, rebuild: true},
		{name: "patterns reordered", global: []string{"gen/"}, extra: []string{"global/"}, rebuild: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := New(time.Minute, tt.global).GraphNeedsIgnoreRebuild(repo, tt.extra); got != tt.rebuild {
				t.Fatalf("GraphNeedsIgnoreRebuild() = %v, want %v", got, tt.rebuild)
			}
		})
	}
}

// A graph from before the rules were recorded cannot be vouched for, so it is
// rebuilt once; the rebuild writes the record and settles it.
func TestGraphWithoutRecordedIgnoreRulesIsRebuiltOnce(t *testing.T) {
	repo := t.TempDir()
	writeGraphSource(t, repo)

	builder := New(time.Minute, nil)
	if err := builder.Update(context.Background(), repo, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(GraphPath(repo)), ignoreFileName)); err != nil {
		t.Fatal(err)
	}

	if !builder.GraphNeedsIgnoreRebuild(repo, nil) {
		t.Fatal("graph without recorded ignore rules was trusted")
	}

	if err := builder.Update(context.Background(), repo, nil); err != nil {
		t.Fatal(err)
	}
	if builder.GraphNeedsIgnoreRebuild(repo, nil) {
		t.Fatal("rebuilt graph still reports stale ignore rules")
	}
}

// Deciding must not cost a read of the graph: it runs for every repository on
// every refresh, before anything is known to have changed.
func TestGraphNeedsIgnoreRebuildDoesNotReadGraph(t *testing.T) {
	repo := t.TempDir()
	writeGraphSource(t, repo)

	builder := New(time.Minute, nil)
	if err := builder.Update(context.Background(), repo, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GraphPath(repo), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if builder.GraphNeedsIgnoreRebuild(repo, nil) {
		t.Fatal("decision depended on the graph's content")
	}
}
