// Package manager orchestrates repositories: clone, build, refresh, merge,
// and query routing to the native graph query engine.
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
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/lease"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// Manager coordinates registry, git, graphify builds and the native graph
// query engine.
type Manager struct {
	reg    *registry.Registry
	git    *gitops.Git
	gfy    *graphify.Client
	engine *graphquery.Engine
	creds  *credentials.Store

	reposDir   string
	mergedPath string
	vectorsDir string

	// Optional docs+RAG subsystem, held as an atomically swappable bundle so
	// settings changes rebuild the clients live. docsDir resolves a clone path
	// to its markdown docs dir. docsMu guards the bundle.
	docsMu   sync.RWMutex
	docs     *docsBundle
	docsDir  func(repoPath string) string
	settings *settings.Store

	baseCtx context.Context //nolint:containedctx // background lifecycle for async jobs

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	mergeMu sync.Mutex
	wg      sync.WaitGroup

	leases *lease.Manager
}

// docsBundle is an immutable snapshot of the docs/RAG clients. A nil field means
// that capability is disabled. Bundles are swapped atomically by Configure; the
// previous bundle's owned store is closed after a swap.
type docsBundle struct {
	gen   docgen.Generator
	rag   *rag.Service
	store vectorstore.Store // owned; closed on swap
}

// DocsDeps carries the immutable wiring for the docs/RAG subsystem.
type DocsDeps struct {
	// DocsDir resolves a repo clone path to its markdown docs directory
	// (typically config.Config.DocsDir).
	DocsDir func(repoPath string) string
	// VectorsDir is the embedded vector store's data directory.
	VectorsDir string
}

// New creates a Manager. baseCtx bounds background refresh jobs. docs carries the
// docs/RAG wiring; the clients themselves are built later via Configure.
func New(
	baseCtx context.Context,
	reg *registry.Registry,
	git *gitops.Git,
	gfy *graphify.Client,
	engine *graphquery.Engine,
	creds *credentials.Store,
	reposDir, mergedPath string,
	docs DocsDeps,
) *Manager {
	m := &Manager{
		reg:        reg,
		git:        git,
		gfy:        gfy,
		engine:     engine,
		creds:      creds,
		reposDir:   reposDir,
		mergedPath: mergedPath,
		vectorsDir: docs.VectorsDir,
		docsDir:    docs.DocsDir,
		docs:       &docsBundle{}, // empty bundle: docs/rag disabled until Configure
		baseCtx:    baseCtx,
		locks:      map[string]*sync.Mutex{},
	}
	m.leases = lease.New(m.TriggerRefresh)

	return m
}

// currentDocs returns the active docs bundle under the read lock.
func (m *Manager) currentDocs() *docsBundle {
	m.docsMu.RLock()
	defer m.docsMu.RUnlock()

	return m.docs
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
	id, repo, _, err := m.registerRepo(ctx, url, branch)
	if err != nil {
		return nil, err
	}

	m.TriggerRefresh(id)

	return repo, nil
}

// AddRepoWait registers a repository and clones+builds it synchronously, blocking
// until the graph is ready (or the build fails). It returns the final repo record
// so callers get the terminal status directly instead of polling repo_status.
// The clone/build honours ctx, so a caller timeout cancels the operation.
//
// A build failure is reported through the returned record's Status ("error") and
// LastError, not as a Go error, so callers always get the final state back.
func (m *Manager) AddRepoWait(ctx context.Context, url, branch string) (*registry.Repo, error) {
	id, _, _, err := m.registerRepo(ctx, url, branch)
	if err != nil {
		return nil, err
	}

	return m.RefreshWait(ctx, id)
}

// RefreshWait pulls and rebuilds a repository synchronously, blocking until the
// graph is ready (or the build fails), then returns the final repo record.
// A build failure is surfaced via the record's Status/LastError rather than a
// Go error; only unexpected lookup failures return an error.
func (m *Manager) RefreshWait(ctx context.Context, id string) (*registry.Repo, error) {
	// Refresh persists StatusError + LastError on failure, so we ignore the
	// returned error here and read the terminal state back from the registry.
	_ = m.Refresh(ctx, id)

	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if repo == nil {
		return nil, fmt.Errorf("repo %s not found", id)
	}

	return repo, nil
}

// registerRepo parses the url, upserts a pending record if the repo is new, and
// reports whether it already existed. It performs no clone/build itself.
func (m *Manager) registerRepo(ctx context.Context, url, branch string) (id string, repo *registry.Repo, existed bool, err error) {
	id, err = gitops.ParseRepoID(url)
	if err != nil {
		return "", nil, false, err
	}

	if existing, gerr := m.reg.Get(ctx, id); gerr != nil {
		return "", nil, false, gerr
	} else if existing != nil {
		return id, existing, true, nil
	}

	repo = &registry.Repo{
		ID:     id,
		URL:    url,
		Branch: branch,
		Path:   filepath.Join(m.reposDir, filepath.FromSlash(id)),
		Status: registry.StatusPending,
	}
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return "", nil, false, err
	}

	return id, repo, false, nil
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

	m.engine.Invalidate(graphify.GraphPath(repo.Path))

	// Best-effort: drop the repo's vectors from the RAG index. The markdown docs
	// live under repo.Path and are removed with the clone below.
	if d := m.currentDocs(); d.rag != nil {
		if err := d.rag.DeleteRepo(ctx, id); err != nil {
			slog.Error("delete repo from rag index", "repo", id, "error", err)
		}
	}

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
	slog.Info("working on repo", "repo", repo.ID, "commit", shortSHA(repo.LastCommit))

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

		slog.Info("repo already up to date, graph unchanged", "repo", repo.ID, "commit", shortSHA(repo.LastCommit))

		return m.reg.Upsert(ctx, repo)
	}

	repo.Status = registry.StatusBuilding
	repo.LastError = ""
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return err
	}

	slog.Info("building graph", "repo", repo.ID, "path", repo.Path, "commit", shortSHA(repo.LastCommit))

	buildStart := time.Now()

	if err := m.gfy.Update(ctx, repo.Path); err != nil {
		return fmt.Errorf("graphify update; %w", err)
	}

	repo.Status = registry.StatusReady
	repo.LastBuildAt = time.Now()
	repo.LastError = ""
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return err
	}

	slog.Info("repo parsed successfully, graph ready",
		"repo", repo.ID,
		"commit", shortSHA(repo.LastCommit),
		"took", time.Since(buildStart).Round(time.Millisecond).String(),
	)

	if err := m.rebuildMerged(ctx); err != nil {
		slog.Error("rebuild merged graph", "error", err)
	}

	// Best-effort docs + RAG indexing. Failures never fail the graph build.
	m.buildDocsAndIndex(ctx, repo)

	return nil
}

// buildDocsAndIndex regenerates markdown docs and refreshes the RAG index for a
// repo. Both steps are optional and best-effort: a nil generator/service or an
// error is logged and swallowed so the graph build result stands.
func (m *Manager) buildDocsAndIndex(ctx context.Context, repo *registry.Repo) {
	d := m.currentDocs()
	if d.gen == nil && d.rag == nil {
		return
	}

	if m.docsDir == nil {
		slog.Error("docs enabled but docsDir resolver is nil; skipping", "repo", repo.ID)

		return
	}

	docsDir := m.docsDir(repo.Path)

	if d.gen != nil {
		if _, err := d.gen.Generate(ctx, repo.ID, repo.Path, docsDir); err != nil {
			slog.Error("generate docs", "repo", repo.ID, "error", err)

			return // no fresh docs -> skip indexing
		}
	}

	if d.rag != nil {
		if err := d.rag.Index(ctx, repo.ID, docsDir); err != nil {
			slog.Error("index docs for rag", "repo", repo.ID, "error", err)
		}
	}
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

		slog.Info("cloning repo", "repo", repo.ID, "url", repo.URL, "branch", repo.Branch)

		cloneStart := time.Now()

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

		slog.Info("repo cloned successfully",
			"repo", repo.ID,
			"commit", shortSHA(head),
			"took", time.Since(cloneStart).Round(time.Millisecond).String(),
		)

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

	slog.Info("new commits on remote, pulling",
		"repo", repo.ID,
		"local", shortSHA(local),
		"remote", shortSHA(remote),
	)

	if err := m.git.Pull(ctx, repo.Path, auth); err != nil {
		return false, fmt.Errorf("pull; %w", err)
	}

	head, err := m.git.Head(ctx, repo.Path)
	if err != nil {
		return false, err
	}

	repo.LastCommit = head

	slog.Info("repo updated to latest", "repo", repo.ID, "commit", shortSHA(head))

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

// CallGraphTool answers a graph query tool call against the resolved graph using
// the in-process native engine (no python serve process is spawned).
func (m *Manager) CallGraphTool(ctx context.Context, repoID, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	graphPath, err := m.GraphPathFor(ctx, repoID)
	if err != nil {
		return nil, err
	}

	text, err := m.engine.Call(graphPath, tool, args)
	if err != nil {
		return nil, err
	}

	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
}

// GraphEngine exposes the native graph engine for in-process consumers (docgen).
func (m *Manager) GraphEngine() *graphquery.Engine { return m.engine }

// repoCloneDir resolves a repo id to its on-disk clone directory, verifying the
// repo is tracked and has actually been cloned.
func (m *Manager) repoCloneDir(ctx context.Context, repoID string) (string, error) {
	repo, err := m.reg.Get(ctx, repoID)
	if err != nil {
		return "", err
	}

	if repo == nil {
		return "", fmt.Errorf("repo %s not found", repoID)
	}

	if repo.Path == "" || !fileExists(filepath.Join(repo.Path, ".git")) {
		return "", fmt.Errorf("repo %s not cloned yet (status: %s)", repoID, repo.Status)
	}

	return repo.Path, nil
}

// ReadRepoFile returns the contents of a source file inside a tracked repo's
// clone. Access is sandboxed to the clone directory. offset/maxBytes paginate
// large files; maxBytes<=0 uses the repofs default cap.
func (m *Manager) ReadRepoFile(ctx context.Context, repoID, relPath string, offset int64, maxBytes int) (*repofs.FileContent, error) {
	dir, err := m.repoCloneDir(ctx, repoID)
	if err != nil {
		return nil, err
	}

	return repofs.ReadFile(dir, relPath, offset, maxBytes)
}

// ListRepoFiles lists files under subdir ("" = repo root) in a tracked repo's
// clone. When recursive is true it walks the whole subtree.
func (m *Manager) ListRepoFiles(ctx context.Context, repoID, subdir string, recursive bool) ([]repofs.Entry, error) {
	dir, err := m.repoCloneDir(ctx, repoID)
	if err != nil {
		return nil, err
	}

	return repofs.ListFiles(dir, subdir, recursive)
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

// shortSHA trims a git commit hash to its 12-char prefix for readable logs.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}

	return sha
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
