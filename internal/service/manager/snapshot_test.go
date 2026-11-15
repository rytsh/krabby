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
	"github.com/rytsh/krabby/internal/service/graphbuilder"
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

	graphBuilder := graphbuilder.New(time.Minute, nil)

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

	measured := newMetricsHarness(t)
	m := &Manager{
		reg: reg, git: git, graphBuilder: graphBuilder, engine: graphquery.NewEngine(0), reposDir: reposDir,
		activity: map[string]map[string]struct{}{},
		metrics:  measured.rec,
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

	if err := m.buildGraphSnapshot(context.Background(), repo, snapshot, registry.StatusReady, nil); err != nil {
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
	if _, err := os.Stat(graphbuilder.GraphPath(persisted.Path)); err != nil {
		t.Fatalf("activated graph missing: %v", err)
	}
	if !graphBuilder.GraphBuiltWithCurrentVersion(persisted.Path) {
		t.Fatal("activated graph is missing its bag engine version marker")
	}
	// The graph stage bypasses runStage, so its measurement is its own.
	if got := measured.counts("krabby.stage.duration"); got["outcome=ok,stage=graph"] != 1 {
		t.Fatalf("graph stage measurements = %v, want one successful build", got)
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
		if err := m.buildGraphSnapshot(context.Background(), repo, next, registry.StatusReady, nil); err != nil {
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

	graphBuilder := graphbuilder.New(time.Minute, nil)

	activePath := filepath.Join(dataDir, "repos", "owner", "repo")
	stagingPath := filepath.Join(dataDir, "repos", ".snapshots", "owner", "repo", ".staging-test")
	if err := os.MkdirAll(stagingPath, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := &registry.Repo{ID: "owner/repo", Path: activePath, LastCommit: "old", Status: registry.StatusReady}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	m := &Manager{reg: reg, graphBuilder: graphBuilder, engine: graphquery.NewEngine(0), reposDir: filepath.Join(dataDir, "repos"), activity: map[string]map[string]struct{}{}}
	snapshot := &preparedSnapshot{StagingPath: stagingPath, FinalPath: stagingPath + "-final", Commit: "new"}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.buildGraphSnapshot(canceled, repo, snapshot, registry.StatusReady, nil); err == nil {
		t.Fatal("buildGraphSnapshot succeeded with a canceled context")
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

// The snapshot root and legacy path are the repository id joined onto shared
// directories. Cleanup must delete only what it created there: an id that
// names a group ("github.com/acme") owns the directories of every repository
// in it, and those are not its versions.
func TestCleanupSnapshotsDeletesOnlyItsOwnVersions(t *testing.T) {
	reposDir := t.TempDir()
	m := &Manager{reposDir: reposDir, engine: graphquery.NewEngine(0)}
	old := time.Now().Add(-snapshotGracePeriod - time.Minute)

	mkdir := func(parts ...string) string {
		t.Helper()
		p := filepath.Join(append([]string{reposDir}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}

		return p
	}

	// Two repositories of the group, each with its own snapshot directory.
	sibling := mkdir(".snapshots", "github.com", "acme", "api")
	other := mkdir(".snapshots", "github.com", "acme", "web")
	// Two versions of the group "repository" itself, both old.
	stale := mkdir(".snapshots", "github.com", "acme", "1600000000000000000-def456")
	newest := mkdir(".snapshots", "github.com", "acme", "1700000000000000000-abc123")
	newer := old.Add(time.Second)
	if err := os.Chtimes(newest, newer, newer); err != nil {
		t.Fatal(err)
	}
	// A legacy-layout clone of a group repository: a directory, not a clone.
	legacyGroup := mkdir("github.com", "acme")

	m.cleanupSnapshots("github.com/acme", "")

	for _, keep := range []string{sibling, other, legacyGroup} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was deleted: %v", keep, err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the group's own old version %s was kept: %v", stale, err)
	}

	// An id that climbs out of the snapshots directory is refused outright.
	m.cleanupSnapshots("x/../..", "")
	if _, err := os.Stat(filepath.Join(reposDir, ".snapshots")); err != nil {
		t.Fatalf("the snapshots directory itself was deleted: %v", err)
	}
}

// Adding a repository whose id contains, or sits inside, a tracked one is
// refused: their per-repository directories would overlap.
func TestAddRepoRefusesNestedIDs(t *testing.T) {
	m, _ := readSymbolManager(t, map[string]string{"a.go": "package a\n"})

	for _, url := range []string{"https://example.com/team", "https://example.com/team/billing/sub"} {
		if _, _, _, err := m.registerRepo(context.Background(), RepoSpec{URL: url}); err == nil {
			t.Errorf("registerRepo(%s) succeeded next to example.com/team/billing", url)
		}
	}
	if _, _, _, err := m.registerRepo(context.Background(), RepoSpec{URL: "https://example.com/team/billing-v2"}); err != nil {
		t.Errorf("a sibling whose name only shares a prefix was refused: %v", err)
	}
}

// After the upstream branch is rewritten a fast-forward is impossible. The
// staging clone is private, so the new snapshot takes the remote's history
// as it now is, instead of failing every refresh from then on.
func TestSnapshotFollowsAForcePushedBranch(t *testing.T) {
	dataDir := t.TempDir()
	remote, work := snapshotTestRemote(t, dataDir)
	reposDir := filepath.Join(dataDir, "repos")
	activePath := filepath.Join(reposDir, "example.com", "team", "repo")
	git := gitops.New("")
	if err := git.Clone(context.Background(), remote, "main", activePath, nil); err != nil {
		t.Fatal(err)
	}

	snapshotTestWrite(t, filepath.Join(work, "version.txt"), "rewritten\n")
	snapshotTestGit(t, work, "add", "version.txt")
	snapshotTestGit(t, work, "commit", "--amend", "-m", "rewritten history")
	snapshotTestGit(t, work, "push", "--force", "origin", "main")

	m := &Manager{git: git, engine: graphquery.NewEngine(0), reposDir: reposDir}
	repo := &registry.Repo{ID: "example.com/team/repo", URL: remote, Branch: "main", Path: activePath}

	if err := git.Fetch(context.Background(), activePath, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.createSnapshot(context.Background(), repo, nil, true, true)
	if err != nil {
		t.Fatalf("snapshot after a force-push: %v", err)
	}
	if got := snapshotTestRead(t, filepath.Join(snapshot.StagingPath, "version.txt")); got != "rewritten\n" {
		t.Fatalf("snapshot content = %q, want the rewritten history", got)
	}
	if got := snapshotTestRead(t, filepath.Join(activePath, "version.txt")); got != "old\n" {
		t.Fatalf("active clone changed: %q", got)
	}
}
