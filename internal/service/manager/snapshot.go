package manager

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/registry"
)

// snapshotGracePeriod is how long a retired repository version is kept after a
// newer one is activated (beyond the always-kept newest previous version). It
// only needs to outlive an in-flight paginated read: a client replaying an
// already-reaped snapshot token transparently falls back to the current active
// version (see repoCloneDirAt), so this stays short.
const snapshotGracePeriod = 5 * time.Minute

type preparedSnapshot struct {
	StagingPath string
	FinalPath   string
	Commit      string
}

// prepareSnapshot fetches remote state without changing the active working
// tree. When a rebuild is needed it creates a private clone for graph generation;
// the caller publishes that clone only after the graph is complete.
func (m *Manager) prepareSnapshot(ctx context.Context, repo *registry.Repo, force bool) (*preparedSnapshot, error) {
	auth, err := m.creds.Resolve(ctx, repo.URL)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials; %w", err)
	}

	hasActiveClone := fileExists(filepath.Join(repo.Path, ".git"))
	if hasActiveClone {
		if err := m.git.Fetch(ctx, repo.Path, auth); err != nil {
			return nil, fmt.Errorf("fetch; %w", err)
		}

		local, err := m.git.Head(ctx, repo.Path)
		if err != nil {
			return nil, err
		}

		remote, err := m.git.RemoteHead(ctx, repo.Path, repo.Branch)
		if err != nil {
			return nil, err
		}

		if local == remote && !force {
			repo.LastCommit = local

			return nil, nil
		}

		slog.Info("new snapshot required",
			"repo", repo.ID,
			"local", shortSHA(local),
			"remote", shortSHA(remote),
			"forced", force,
		)
	} else {
		repo.Status = registry.StatusCloning
		if err := m.reg.Upsert(ctx, repo); err != nil {
			return nil, err
		}
	}

	return m.createSnapshot(ctx, repo, auth, hasActiveClone, true)
}

// prepareCurrentSnapshot copies the active commit without contacting the
// remote. Selective graph generation therefore keeps its existing no-sync
// contract while still avoiding writes to the published snapshot.
func (m *Manager) prepareCurrentSnapshot(ctx context.Context, repo *registry.Repo) (*preparedSnapshot, error) {
	if !fileExists(filepath.Join(repo.Path, ".git")) {
		return nil, fmt.Errorf("repo %s has no clone yet; refresh it first", repo.ID)
	}

	return m.createSnapshot(ctx, repo, nil, true, false)
}

func (m *Manager) createSnapshot(
	ctx context.Context,
	repo *registry.Repo,
	auth *credentials.Auth,
	hasActiveClone, syncRemote bool,
) (_ *preparedSnapshot, retErr error) {
	root := m.snapshotRoot(repo.ID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir snapshot root; %w", err)
	}

	m.cleanupSnapshots(repo.ID, repo.Path)

	stagingPath, err := os.MkdirTemp(root, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("create snapshot staging directory; %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(stagingPath)
		}
	}()

	cloneURL := repo.URL
	cloneAuth := auth
	if hasActiveClone {
		cloneURL = repo.Path
		cloneAuth = nil
	}

	cloneStart := time.Now()
	if err := m.git.Clone(ctx, cloneURL, repo.Branch, stagingPath, cloneAuth); err != nil {
		return nil, fmt.Errorf("clone snapshot; %w", err)
	}

	if hasActiveClone {
		if err := m.git.SetRemoteURL(ctx, stagingPath, repo.URL); err != nil {
			return nil, fmt.Errorf("set snapshot origin; %w", err)
		}
		if syncRemote {
			if err := m.git.Fetch(ctx, stagingPath, auth); err != nil {
				return nil, fmt.Errorf("fetch snapshot; %w", err)
			}
			if err := m.git.Pull(ctx, stagingPath, auth); err != nil {
				return nil, fmt.Errorf("update snapshot; %w", err)
			}
		}
	}

	head, err := m.git.Head(ctx, stagingPath)
	if err != nil {
		return nil, err
	}

	finalPath := filepath.Join(root, fmt.Sprintf("%d-%s", time.Now().UnixNano(), shortSHA(head)))
	slog.Info("repo snapshot prepared",
		"repo", repo.ID,
		"commit", shortSHA(head),
		"took", time.Since(cloneStart).Round(time.Millisecond).String(),
	)

	return &preparedSnapshot{StagingPath: stagingPath, FinalPath: finalPath, Commit: head}, nil
}

// buildGraphSnapshot builds privately, then activates source and graph together
// with one registry record replacement. A failed build leaves the old Path and
// LastCommit untouched.
func (m *Manager) buildGraphSnapshot(
	ctx context.Context,
	repo *registry.Repo,
	snapshot *preparedSnapshot,
	activeStatus string,
	skips runSkips,
) error {
	// The graph stage may be disabled, but this function also promotes the
	// staging clone to its final path and moves repo.Path/LastCommit forward —
	// which every other stage depends on. So a skip bypasses only the graph
	// work and its stage bookkeeping, never the promotion below.
	skipGraph := skips.skips(repo, registry.StageGraph)

	st := repo.Stages.Get(registry.StageGraph)
	fail := func(err error, path string) error {
		if ctx.Err() != nil {
			err = ErrCancelled
		}
		st.Status = registry.StageError
		st.Error = err.Error()
		st.Commit = snapshot.Commit
		st.FinishedAt = time.Now()
		// Logged rather than returned: err is the failure the caller needs to
		// see, but a lost write leaves the stage looking like it never failed.
		if upErr := m.reg.Upsert(context.WithoutCancel(ctx), repo); upErr != nil {
			slog.Error("persist graph stage failure", "repo", repo.ID, "error", upErr)
		}
		if rmErr := os.RemoveAll(path); rmErr != nil {
			slog.Warn("remove failed graph output", "path", path, "error", rmErr)
		}

		return err
	}

	start := time.Now()

	if skipGraph {
		slog.Info("graph stage disabled for repo; promoting snapshot without building a graph", "repo", repo.ID)
	} else {
		st.Status = registry.StageRunning
		st.Error = ""
		if err := m.reg.Upsert(ctx, repo); err != nil {
			return fail(err, snapshot.StagingPath)
		}

		m.setActivity(repo.ID, registry.StageGraph)
		defer m.clearActivity(repo.ID, registry.StageGraph)

		if err := m.graphBuilder.Update(ctx, snapshot.StagingPath, repo.Overrides.GraphExclude); err != nil {
			return fail(err, snapshot.StagingPath)
		}
		if err := graphquery.Validate(graphbuilder.GraphPath(snapshot.StagingPath)); err != nil {
			return fail(fmt.Errorf("validate graph output; %w", err), snapshot.StagingPath)
		}
		if err := m.graphBuilder.RecordGraphVersion(snapshot.StagingPath); err != nil {
			return fail(err, snapshot.StagingPath)
		}
	}

	if err := os.Rename(snapshot.StagingPath, snapshot.FinalPath); err != nil {
		return fail(fmt.Errorf("finalize snapshot; %w", err), snapshot.StagingPath)
	}
	_ = os.Chtimes(snapshot.FinalPath, time.Now(), time.Now())

	oldPath := repo.Path
	oldCommit := repo.LastCommit
	oldStatus := repo.Status
	oldBuildAt := repo.LastBuildAt
	oldError := repo.LastError
	if oldPath != "" {
		_ = os.Chtimes(oldPath, time.Now(), time.Now())
	}

	repo.Path = snapshot.FinalPath
	repo.LastCommit = snapshot.Commit
	repo.Status = activeStatus
	repo.LastBuildAt = time.Now()
	repo.LastError = ""

	// A skipped stage records no state, so status output shows it as never run
	// rather than as a success that produced nothing.
	if !skipGraph {
		st.Status = registry.StageOK
		st.Error = ""
		st.Commit = snapshot.Commit
		st.FinishedAt = time.Now()
	}

	if err := m.reg.Upsert(ctx, repo); err != nil {
		repo.Path = oldPath
		repo.LastCommit = oldCommit
		repo.Status = oldStatus
		repo.LastBuildAt = oldBuildAt
		repo.LastError = oldError

		return fail(err, snapshot.FinalPath)
	}

	slog.Info("graph snapshot activated", "repo", repo.ID, "commit", shortSHA(snapshot.Commit),
		"took", time.Since(start).Round(time.Millisecond).String())
	m.cleanupSnapshots(repo.ID, repo.Path)

	return nil
}

func (m *Manager) snapshotRoot(id string) string {
	return filepath.Join(m.reposDir, ".snapshots", filepath.FromSlash(id))
}

func (m *Manager) legacyRepoPath(id string) string {
	return filepath.Join(m.reposDir, filepath.FromSlash(id))
}

// cleanupSnapshots keeps the active and newest previous snapshot. Older
// versions are reaped once snapshotGracePeriod has elapsed, without delaying
// refresh or mutating a published tree.
func (m *Manager) cleanupSnapshots(id, activePath string) {
	root := m.snapshotRoot(id)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}

	type version struct {
		path string
		mod  time.Time
	}
	var versions []version
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if strings.HasPrefix(entry.Name(), ".staging-") {
			if filepath.Clean(path) != filepath.Clean(activePath) {
				_ = os.RemoveAll(path)
			}
			continue
		}
		if !entry.IsDir() || filepath.Clean(path) == filepath.Clean(activePath) {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			versions = append(versions, version{path: path, mod: info.ModTime()})
		}
	}

	sort.Slice(versions, func(i, j int) bool { return versions[i].mod.After(versions[j].mod) })
	for i, version := range versions {
		if i == 0 || time.Since(version.mod) < snapshotGracePeriod {
			continue
		}
		m.engine.Invalidate(graphbuilder.GraphPath(version.path))
		if err := os.RemoveAll(version.path); err != nil {
			slog.Warn("remove retired snapshot", "repo", id, "path", version.path, "error", err)
		}
	}

	legacyPath := m.legacyRepoPath(id)
	if filepath.Clean(legacyPath) == filepath.Clean(activePath) {
		return
	}
	if info, err := os.Stat(legacyPath); err == nil && time.Since(info.ModTime()) >= snapshotGracePeriod {
		m.engine.Invalidate(graphbuilder.GraphPath(legacyPath))
		if err := os.RemoveAll(legacyPath); err != nil {
			slog.Warn("remove retired legacy clone", "repo", id, "path", legacyPath, "error", err)
		}
	}
}

// repoCloneDirAt resolves an optional snapshot token. A token is a soft hint,
// not a hard requirement: while the pinned immutable version still exists it is
// honored so paginated reads stay on one commit, but an unknown, malformed, or
// already-retired token transparently falls back to the current active snapshot
// (returning its token) so a client that keeps replaying an old token never
// wedges once the grace period reaps that version.
func (m *Manager) repoCloneDirAt(ctx context.Context, repoID, snapshot string) (string, string, error) {
	repo, err := m.reg.Get(ctx, repoID)
	if err != nil {
		return "", "", err
	}

	if repo == nil {
		return "", "", fmt.Errorf("repo %s not found", repoID)
	}

	// A pinned token: honor it only while that version is still on disk. A
	// traversal-unsafe token is ignored (never joined into a path) and simply
	// falls through to the current active snapshot.
	if snapshot != "" && snapshot == filepath.Base(snapshot) && !strings.ContainsAny(snapshot, `/\`) &&
		snapshot != m.snapshotToken(repoID, repo.Path) {
		dir := filepath.Join(m.snapshotRoot(repoID), snapshot)
		if snapshot == "legacy" {
			dir = m.legacyRepoPath(repoID)
		}
		if fileExists(filepath.Join(dir, ".git")) {
			return dir, snapshot, nil
		}
	}

	// Current active snapshot (empty, matching, or retired/unknown token).
	if repo.Path == "" || !fileExists(filepath.Join(repo.Path, ".git")) {
		return "", "", fmt.Errorf("repo %s not cloned yet (status: %s)", repoID, repo.Status)
	}

	return repo.Path, m.snapshotToken(repoID, repo.Path), nil
}

func (m *Manager) snapshotToken(repoID, repoPath string) string {
	if pathWithin(repoPath, m.snapshotRoot(repoID)) {
		return filepath.Base(repoPath)
	}

	return "legacy"
}
