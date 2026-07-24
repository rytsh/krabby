package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

func TestSnapshotActivationLeavesActiveCloneUntouchedUntilPublish(t *testing.T) {
	dataDir := t.TempDir()
	remote, work := snapshotTestRemote(t, dataDir)
	reposDir := filepath.Join(dataDir, "repos")
	activePath := filepath.Join(reposDir, "example.com", "team", "repo")
	git := gitops.New("")
	if err := git.Clone(context.Background(), remote, "main", activePath, nil); err != nil {
		t.Fatal(err)
	}
	oldHead, err := git.Head(context.Background(), activePath)
	if err != nil {
		t.Fatal(err)
	}

	snapshotTestWrite(t, filepath.Join(work, "version.txt"), "new\n")
	snapshotTestGit(t, work, "add", "version.txt")
	snapshotTestGit(t, work, "commit", "-m", "new version")
	snapshotTestGit(t, work, "push", "origin", "main")

	db, err := storage.Open(filepath.Join(dataDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	graphifyBin := filepath.Join(dataDir, "graphify-test")
	snapshotTestWrite(t, graphifyBin, "#!/bin/sh\nmkdir -p \"$2/graphify-out\"\nprintf '%s' '{\"nodes\":[],\"links\":[]}' > \"$2/graphify-out/graph.json\"\n")
	if err := os.Chmod(graphifyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	gfy, err := graphify.New(graphifyBin, "sh", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}

	repo := &registry.Repo{
		ID:         "example.com/team/repo",
		URL:        remote,
		Branch:     "main",
		Path:       activePath,
		LastCommit: oldHead,
		Status:     registry.StatusReady,
	}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		reg: reg, git: git, gfy: gfy, engine: graphquery.NewEngine(0), reposDir: reposDir,
		activity: map[string]map[string]struct{}{},
	}
	oldRead, err := m.ReadRepoFileAt(context.Background(), repo.ID, "version.txt", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if oldRead.Snapshot != "legacy" {
		t.Fatalf("legacy snapshot token = %q", oldRead.Snapshot)
	}
	snapshot, err := m.createSnapshot(context.Background(), repo, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}

	if got := snapshotTestRead(t, filepath.Join(activePath, "version.txt")); got != "old\n" {
		t.Fatalf("active clone changed before publish: %q", got)
	}
	if got := snapshotTestRead(t, filepath.Join(snapshot.StagingPath, "version.txt")); got != "new\n" {
		t.Fatalf("prepared snapshot = %q, want new version", got)
	}

	if err := m.buildGraphSnapshot(context.Background(), repo, snapshot, registry.StatusReady); err != nil {
		t.Fatal(err)
	}
	persisted, err := reg.Get(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Path != snapshot.FinalPath || persisted.LastCommit != snapshot.Commit {
		t.Fatalf("activated repo = path %q commit %q, want %q %q", persisted.Path, persisted.LastCommit, snapshot.FinalPath, snapshot.Commit)
	}
	if got := snapshotTestRead(t, filepath.Join(activePath, "version.txt")); got != "old\n" {
		t.Fatalf("old snapshot changed after publish: %q", got)
	}
	if !strings.HasPrefix(persisted.Path, m.snapshotRoot(repo.ID)+string(filepath.Separator)) {
		t.Fatalf("active path %q is outside snapshot root", persisted.Path)
	}
	if _, err := os.Stat(graphify.GraphPath(persisted.Path)); err != nil {
		t.Fatalf("activated graph missing: %v", err)
	}

	pinned, err := m.ReadRepoFileAt(context.Background(), repo.ID, "version.txt", oldRead.Snapshot, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Content != "old\n" {
		t.Fatalf("pinned continuation read = %q, want old version", pinned.Content)
	}
	current, err := m.ReadRepoFile(context.Background(), repo.ID, "version.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if current.Content != "new\n" || current.Snapshot == oldRead.Snapshot {
		t.Fatalf("current read = content %q snapshot %q", current.Content, current.Snapshot)
	}

	// A retired or unknown token must transparently fall back to the current
	// active snapshot (and hand back the current token) instead of erroring, so
	// an MCP client replaying a stale token keeps working.
	fallback, err := m.ReadRepoFileAt(context.Background(), repo.ID, "version.txt", "1700000000000000000-deadbeef", 0, 0)
	if err != nil {
		t.Fatalf("stale snapshot token did not fall back to current: %v", err)
	}
	if fallback.Content != "new\n" || fallback.Snapshot != current.Snapshot {
		t.Fatalf("fallback read = content %q snapshot %q, want current", fallback.Content, fallback.Snapshot)
	}

	oldTime := time.Now().Add(-snapshotGracePeriod - time.Minute)
	firstSnapshotPath := persisted.Path
	firstSnapshotToken := current.Snapshot
	if err := os.Chtimes(firstSnapshotPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		next, err := m.prepareCurrentSnapshot(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.buildGraphSnapshot(context.Background(), repo, next, registry.StatusReady); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.ReadRepoFileAt(context.Background(), repo.ID, "version.txt", firstSnapshotToken, 0, 0); err != nil {
		t.Fatalf("recently retired snapshot was cleaned before grace period: %v", err)
	}

	if err := os.Chtimes(activePath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	m.cleanupSnapshots(repo.ID, persisted.Path)
	if _, err := os.Stat(activePath); !os.IsNotExist(err) {
		t.Fatalf("retired legacy clone still exists: %v", err)
	}
}

func TestSnapshotBuildFailureKeepsActivePathAndCleansStaging(t *testing.T) {
	dataDir := t.TempDir()
	db, err := storage.Open(filepath.Join(dataDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dataDir, "graphify-fail")
	snapshotTestWrite(t, bin, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gfy, err := graphify.New(bin, "sh", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}

	activePath := filepath.Join(dataDir, "repos", "owner", "repo")
	stagingPath := filepath.Join(dataDir, "repos", ".snapshots", "owner", "repo", ".staging-test")
	if err := os.MkdirAll(stagingPath, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := &registry.Repo{ID: "owner/repo", Path: activePath, LastCommit: "old", Status: registry.StatusReady}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	m := &Manager{reg: reg, gfy: gfy, engine: graphquery.NewEngine(0), reposDir: filepath.Join(dataDir, "repos"), activity: map[string]map[string]struct{}{}}
	snapshot := &preparedSnapshot{StagingPath: stagingPath, FinalPath: stagingPath + "-final", Commit: "new"}
	if err := m.buildGraphSnapshot(context.Background(), repo, snapshot, registry.StatusReady); err == nil {
		t.Fatal("buildGraphSnapshot succeeded with failing graphify")
	}

	persisted, err := reg.Get(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Path != activePath || persisted.LastCommit != "old" {
		t.Fatalf("failed build activated snapshot: path=%q commit=%q", persisted.Path, persisted.LastCommit)
	}
	if persisted.Stages.Graph.Status != registry.StageError {
		t.Fatalf("graph stage status = %q, want error", persisted.Stages.Graph.Status)
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("failed staging directory remains: %v", err)
	}
}

func snapshotTestRemote(t *testing.T, root string) (remote, work string) {
	t.Helper()
	remote = filepath.Join(root, "remote.git")
	work = filepath.Join(root, "work")
	snapshotTestGit(t, root, "init", "--bare", remote)
	snapshotTestGit(t, root, "init", "-b", "main", work)
	snapshotTestGit(t, work, "config", "user.email", "test@example.com")
	snapshotTestGit(t, work, "config", "user.name", "Snapshot Test")
	snapshotTestGit(t, work, "config", "commit.gpgsign", "false")
	snapshotTestWrite(t, filepath.Join(work, "version.txt"), "old\n")
	snapshotTestGit(t, work, "add", "version.txt")
	snapshotTestGit(t, work, "commit", "-m", "old version")
	snapshotTestGit(t, work, "remote", "add", "origin", remote)
	snapshotTestGit(t, work, "push", "-u", "origin", "main")

	return remote, work
}

func snapshotTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func snapshotTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshotTestRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}
