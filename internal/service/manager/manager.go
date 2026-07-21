// Package manager orchestrates repositories: clone, build, refresh, merge,
// and query routing to the servepool.
package manager

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/lease"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/servepool"
)

// Manager coordinates registry, git, graphify builds and the serve pool.
type Manager struct {
	reg   *registry.Registry
	git   *gitops.Git
	gfy   *graphify.Client
	pool  *servepool.Pool
	creds *credentials.Store

	reposDir   string
	mergedPath string

	baseCtx context.Context //nolint:containedctx // background lifecycle for async jobs

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	mergeMu sync.Mutex
	wg      sync.WaitGroup

	leases *lease.Manager
}

// New creates a Manager. baseCtx bounds background refresh jobs.
func New(
	baseCtx context.Context,
	reg *registry.Registry,
	git *gitops.Git,
	gfy *graphify.Client,
	pool *servepool.Pool,
	creds *credentials.Store,
	reposDir, mergedPath string,
) *Manager {
	m := &Manager{
		reg:        reg,
		git:        git,
		gfy:        gfy,
		pool:       pool,
		creds:      creds,
		reposDir:   reposDir,
		mergedPath: mergedPath,
		baseCtx:    baseCtx,
		locks:      map[string]*sync.Mutex{},
	}
	m.leases = lease.New(m.TriggerRefresh)

	return m
}

// Credentials exposes the credential store for API and MCP handlers.
func (m *Manager) Credentials() *credentials.Store { return m.creds }

// Wait blocks until in-flight background jobs finish.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) lock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	if l, ok := m.locks[id]; ok {
		return l
	}

	l := &sync.Mutex{}
	m.locks[id] = l

	return l
}

// AddRepo registers a repository and starts a background clone+build.
// If the repo already exists, it just triggers a refresh.
func (m *Manager) AddRepo(ctx context.Context, url, branch string) (*registry.Repo, error) {
	id, err := gitops.ParseRepoID(url)
	if err != nil {
		return nil, err
	}

	if existing, err := m.reg.Get(ctx, id); err != nil {
		return nil, err
	} else if existing != nil {
		m.TriggerRefresh(id)

		return existing, nil
	}

	repo := &registry.Repo{
		ID:     id,
		URL:    url,
		Branch: branch,
		Path:   filepath.Join(m.reposDir, filepath.FromSlash(id)),
		Status: registry.StatusPending,
	}
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return nil, err
	}

	m.TriggerRefresh(id)

	return repo, nil
}

// RemoveRepo deletes the record, the local clone and its serve process.
func (m *Manager) RemoveRepo(ctx context.Context, id string) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return err
	}

	if repo == nil {
		return fmt.Errorf("repo %s not found", id)
	}

	m.pool.Invalidate(graphify.GraphPath(repo.Path))

	if err := m.reg.Delete(ctx, id); err != nil {
		return err
	}

	if repo.Path != "" && filepath.HasPrefix(repo.Path, m.reposDir) {
		if err := os.RemoveAll(repo.Path); err != nil {
			return fmt.Errorf("remove clone %s; %w", repo.Path, err)
		}
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		if err := m.rebuildMerged(m.baseCtx); err != nil {
			slog.Error("rebuild merged graph", "error", err)
		}
	}()

	return nil
}

// TriggerRefresh starts a background refresh for a repo. Concurrent triggers
// for the same repo serialize on the per-repo lock.
func (m *Manager) TriggerRefresh(id string) {
	m.wg.Add(1)

	go func() {
		defer m.wg.Done()

		if err := m.Refresh(m.baseCtx, id); err != nil {
			slog.Error("refresh repo", "repo", id, "error", err)
		}
	}()
}

// Refresh synchronously clones/pulls the repo and rebuilds its graph if needed.
func (m *Manager) Refresh(ctx context.Context, id string) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return err
	}

	if repo == nil {
		return fmt.Errorf("repo %s not found", id)
	}

	// An external tool holds a read lease: defer the refresh until release/expiry.
	if m.leases.Active(id) {
		m.leases.MarkPending(id)

		slog.Info("refresh deferred: repo is leased", "repo", id)

		return nil
	}

	if err := m.refresh(ctx, repo); err != nil {
		repo.Status = registry.StatusError
		repo.LastError = err.Error()

		if uerr := m.reg.Upsert(ctx, repo); uerr != nil {
			slog.Error("save repo error state", "repo", id, "error", uerr)
		}

		return err
	}

	return nil
}

func (m *Manager) refresh(ctx context.Context, repo *registry.Repo) error {
	graphPath := graphify.GraphPath(repo.Path)
	hadGraph := fileExists(graphPath)

	changed, err := m.sync(ctx, repo)
	if err != nil {
		return err
	}

	repo.LastSyncAt = time.Now()

	if hadGraph && !changed {
		// Nothing new; keep current status.
		repo.Status = registry.StatusReady
		repo.LastError = ""

		return m.reg.Upsert(ctx, repo)
	}

	repo.Status = registry.StatusBuilding
	repo.LastError = ""
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return err
	}

	slog.Info("building graph", "repo", repo.ID, "path", repo.Path)

	if err := m.gfy.Update(ctx, repo.Path); err != nil {
		return fmt.Errorf("graphify update; %w", err)
	}

	repo.Status = registry.StatusReady
	repo.LastBuildAt = time.Now()
	repo.LastError = ""
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return err
	}

	if err := m.rebuildMerged(ctx); err != nil {
		slog.Error("rebuild merged graph", "error", err)
	}

	return nil
}

// sync makes the local clone current. It returns true when new commits arrived
// (or the repo was cloned for the first time).
func (m *Manager) sync(ctx context.Context, repo *registry.Repo) (bool, error) {
	auth, err := m.creds.Resolve(ctx, repo.URL)
	if err != nil {
		return false, fmt.Errorf("resolve credentials; %w", err)
	}

	if !fileExists(filepath.Join(repo.Path, ".git")) {
		repo.Status = registry.StatusCloning
		if err := m.reg.Upsert(ctx, repo); err != nil {
			return false, err
		}

		slog.Info("cloning repo", "repo", repo.ID, "url", repo.URL)

		if err := os.MkdirAll(filepath.Dir(repo.Path), 0o755); err != nil {
			return false, fmt.Errorf("mkdir; %w", err)
		}

		if err := m.git.Clone(ctx, repo.URL, repo.Branch, repo.Path, auth); err != nil {
			return false, fmt.Errorf("clone; %w", err)
		}

		head, err := m.git.Head(ctx, repo.Path)
		if err != nil {
			return false, err
		}

		repo.LastCommit = head

		return true, nil
	}

	if err := m.git.Fetch(ctx, repo.Path, auth); err != nil {
		return false, fmt.Errorf("fetch; %w", err)
	}

	local, err := m.git.Head(ctx, repo.Path)
	if err != nil {
		return false, err
	}

	remote, err := m.git.RemoteHead(ctx, repo.Path, repo.Branch)
	if err != nil {
		return false, err
	}

	if local == remote {
		repo.LastCommit = local

		return false, nil
	}

	if err := m.git.Pull(ctx, repo.Path, auth); err != nil {
		return false, fmt.Errorf("pull; %w", err)
	}

	head, err := m.git.Head(ctx, repo.Path)
	if err != nil {
		return false, err
	}

	repo.LastCommit = head

	return true, nil
}

// rebuildMerged regenerates the cross-repo merged graph from all ready repos.
func (m *Manager) rebuildMerged(ctx context.Context) error {
	m.mergeMu.Lock()
	defer m.mergeMu.Unlock()

	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	var graphs []string

	for _, r := range repos {
		gp := graphify.GraphPath(r.Path)
		if fileExists(gp) {
			graphs = append(graphs, gp)
		}
	}

	switch len(graphs) {
	case 0:
		return nil
	case 1:
		// merge-graphs needs >=2 inputs; single repo = copy.
		if err := copyFile(graphs[0], m.mergedPath); err != nil {
			return fmt.Errorf("copy single graph; %w", err)
		}
	default:
		if err := m.gfy.MergeGraphs(ctx, m.mergedPath, graphs...); err != nil {
			return err
		}
	}

	slog.Info("merged graph rebuilt", "repos", len(graphs), "out", m.mergedPath)

	return nil
}

// GraphPathFor resolves a repo id ("" = merged cross-repo graph) to a graph file.
func (m *Manager) GraphPathFor(ctx context.Context, repoID string) (string, error) {
	if repoID == "" {
		if !fileExists(m.mergedPath) {
			return "", fmt.Errorf("merged graph not built yet; add repos and wait for builds")
		}

		return m.mergedPath, nil
	}

	repo, err := m.reg.Get(ctx, repoID)
	if err != nil {
		return "", err
	}

	if repo == nil {
		return "", fmt.Errorf("repo %s not found", repoID)
	}

	gp := graphify.GraphPath(repo.Path)
	if !fileExists(gp) {
		return "", fmt.Errorf("graph for %s not built yet (status: %s)", repoID, repo.Status)
	}

	return gp, nil
}

// CallGraphTool proxies an MCP tool call to the server for the resolved graph.
func (m *Manager) CallGraphTool(ctx context.Context, repoID, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	graphPath, err := m.GraphPathFor(ctx, repoID)
	if err != nil {
		return nil, err
	}

	return m.pool.CallTool(ctx, graphPath, tool, args)
}

// AcquireLease takes a TTL-bounded read lease on a repo so external tools can
// walk its clone without racing a refresh. Fails while a refresh is running.
func (m *Manager) AcquireLease(ctx context.Context, id, owner string, ttl time.Duration) (*lease.Lease, error) {
	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if repo == nil {
		return nil, fmt.Errorf("repo %s not found", id)
	}

	// Ensure no refresh is mutating the clone right now.
	l := m.lock(id)
	if !l.TryLock() {
		return nil, fmt.Errorf("refresh in progress for %s; retry shortly", id)
	}
	l.Unlock()

	return m.leases.Acquire(id, owner, ttl)
}

// ReleaseLease ends a lease; a deferred refresh fires automatically.
func (m *Manager) ReleaseLease(id, token string) error {
	return m.leases.Release(id, token)
}

// LeaseInfo returns the active lease for id without its token, or nil.
func (m *Manager) LeaseInfo(id string) *lease.Lease {
	return m.leases.Info(id)
}

// Registry exposes read access for API handlers.
func (m *Manager) Registry() *registry.Registry { return m.reg }

// MergedPath returns the merged cross-repo graph location ("" until built).
func (m *Manager) MergedPath() string {
	if !fileExists(m.mergedPath) {
		return ""
	}

	return m.mergedPath
}

func fileExists(p string) bool {
	_, err := os.Stat(p)

	return err == nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"

	out, err := os.Create(tmp)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()

		return err
	}

	if err := out.Close(); err != nil {
		return err
	}

	return os.Rename(tmp, dst)
}
