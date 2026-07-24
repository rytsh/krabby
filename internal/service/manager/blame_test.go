package manager

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

// blameTestManager sets up a real git clone with two commits touching different
// line ranges of a file, then returns a Manager wired to it plus the repo id.
func blameTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dataDir := t.TempDir()
	remote, work := snapshotTestRemote(t, dataDir)
	reposDir := filepath.Join(dataDir, "repos")
	activePath := filepath.Join(reposDir, "example.com", "team", "repo")

	// Commit 1: lines 1-3 by the "old version" author (from snapshotTestRemote
	// the file version.txt already exists). Add a source file with 3 lines.
	snapshotTestWrite(t, filepath.Join(work, "code.txt"), "line one\nline two\nline three\n")
	snapshotTestGit(t, work, "add", "code.txt")
	snapshotTestGit(t, work, "commit", "-m", "add code")

	// Commit 2: change only the middle line, by a different author.
	snapshotTestGit(t, work, "config", "user.email", "second@example.com")
	snapshotTestGit(t, work, "config", "user.name", "Second Author")
	snapshotTestWrite(t, filepath.Join(work, "code.txt"), "line one\nline TWO changed\nline three\n")
	snapshotTestGit(t, work, "add", "code.txt")
	snapshotTestGit(t, work, "commit", "-m", "change middle line")
	snapshotTestGit(t, work, "push", "origin", "main")

	git := gitops.New("")
	if err := git.Clone(context.Background(), remote, "main", activePath, nil); err != nil {
		t.Fatal(err)
	}
	head, err := git.Head(context.Background(), activePath)
	if err != nil {
		t.Fatal(err)
	}

	db, err := storage.Open(filepath.Join(dataDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	repo := &registry.Repo{
		ID:         "example.com/team/repo",
		URL:        remote,
		Branch:     "main",
		Path:       activePath,
		LastCommit: head,
		Status:     registry.StatusReady,
	}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	m := &Manager{reg: reg, git: git, reposDir: reposDir}

	return m, repo.ID
}

func TestBlameRepoFileGroupsHunks(t *testing.T) {
	m, repoID := blameTestManager(t)

	res, err := m.BlameRepoFile(context.Background(), repoID, "code.txt", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Line 2 was changed in a later commit, so we expect three hunks:
	// [1], [2], [3] where 1 and 3 share the first commit and 2 is the second.
	if len(res.Hunks) != 3 {
		t.Fatalf("expected 3 hunks, got %d: %#v", len(res.Hunks), res.Hunks)
	}
	if res.Hunks[0].LineStart != 1 || res.Hunks[0].LineEnd != 1 {
		t.Errorf("hunk 0 range = %d-%d, want 1-1", res.Hunks[0].LineStart, res.Hunks[0].LineEnd)
	}
	if res.Hunks[1].LineStart != 2 || res.Hunks[1].LineEnd != 2 {
		t.Errorf("hunk 1 range = %d-%d, want 2-2", res.Hunks[1].LineStart, res.Hunks[1].LineEnd)
	}
	if got := res.Hunks[1].Lines[0]; got != "line TWO changed" {
		t.Errorf("hunk 1 content = %q, want %q", got, "line TWO changed")
	}

	// Hunk 0 and hunk 2 come from the same (first) commit; hunk 1 is different.
	if res.Hunks[0].Commit != res.Hunks[2].Commit {
		t.Errorf("hunks 0 and 2 should share a commit: %q vs %q", res.Hunks[0].Commit, res.Hunks[2].Commit)
	}
	if res.Hunks[1].Commit == res.Hunks[0].Commit {
		t.Errorf("hunk 1 should be a different commit than hunk 0")
	}

	// Commit metadata is deduplicated: exactly two distinct commits, each with
	// its author, referenced by every hunk.
	if len(res.Commits) != 2 {
		t.Fatalf("expected 2 distinct commits, got %d: %#v", len(res.Commits), res.Commits)
	}
	for _, h := range res.Hunks {
		c, ok := res.Commits[h.Commit]
		if !ok {
			t.Fatalf("hunk commit %q missing from Commits map", h.Commit)
		}
		if c.Author == "" || c.Time == 0 {
			t.Errorf("commit %q missing metadata: %#v", h.Commit, c)
		}
	}
	if a := res.Commits[res.Hunks[1].Commit].Author; a != "Second Author" {
		t.Errorf("changed line author = %q, want %q", a, "Second Author")
	}
}

func TestBlameRepoFileLineRange(t *testing.T) {
	m, repoID := blameTestManager(t)

	res, err := m.BlameRepoFile(context.Background(), repoID, "code.txt", "", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hunks) != 1 {
		t.Fatalf("expected 1 hunk for single-line range, got %d", len(res.Hunks))
	}
	if res.Hunks[0].LineStart != 2 || res.Hunks[0].LineEnd != 2 {
		t.Errorf("range hunk = %d-%d, want 2-2", res.Hunks[0].LineStart, res.Hunks[0].LineEnd)
	}
	if res.Start != 2 || res.End != 2 {
		t.Errorf("result range = %d-%d, want 2-2", res.Start, res.End)
	}
}

func TestBlameRepoFileRejectsTraversal(t *testing.T) {
	m, repoID := blameTestManager(t)

	if _, err := m.BlameRepoFile(context.Background(), repoID, "../../etc/passwd", "", 0, 0); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}
