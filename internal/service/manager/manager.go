// Package manager orchestrates repositories: clone, build, refresh, merge,
// and query routing to the native graph query engine.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/coderag"
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
	reg      *registry.Registry
	git      *gitops.Git
	gfy      *graphify.Client
	engine   *graphquery.Engine
	creds    *credentials.Store
	codeText *coderag.TextStore

	reposDir       string
	mergedPath     string
	docsRootDir    string
	docsVectorsDir string
	codeVectorsDir string

	// Optional docs+RAG subsystem, held as an atomically swappable bundle so
	// settings changes rebuild the clients live. docsMu guards the bundle.
	docsMu      sync.RWMutex
	docs        *docsBundle
	settings    *settings.Store
	settingsMu  sync.Mutex
	configureMu sync.Mutex

	baseCtx context.Context //nolint:containedctx // background lifecycle for async jobs

	mu          sync.Mutex
	locks       map[string]*sync.Mutex
	mergeMu     sync.Mutex
	wg          sync.WaitGroup
	lifecycleMu sync.Mutex
	closing     bool

	// activity tracks the currently running pipeline step per repo (transient,
	// in-memory): "sync" or a registry.Stage* name. Empty = idle.
	activityMu sync.Mutex
	activity   map[string]string

	// Effective MCP API key (runtime override or config), cached for the
	// per-request auth check. mcpConfigKey is the startup config value used
	// when the override is cleared.
	mcpKeyMu     sync.RWMutex
	mcpKey       string
	mcpConfigKey string

	leases *lease.Manager
}

// docsBundle is an immutable snapshot of the docs/RAG clients. A nil field means
// that capability is disabled. Bundles are swapped atomically by Configure; the
// previous bundle's owned store is closed after a swap.
type docsBundle struct {
	gen   docgen.Generator
	rag   *rag.Service
	store vectorstore.Store // owned; closed on swap

	codeRag   *coderag.Service
	codeStore vectorstore.Store // owned; closed on swap
}

// DocsDeps carries the immutable wiring for the docs/RAG subsystem.
type DocsDeps struct {
	// DocsRootDir stores generated docs by repo-id path, outside repository
	// clones (typically config.Config.DocsRootDir()).
	DocsRootDir string
	// DocsVectorsDir is the embedded vector store's data directory for docs RAG.
	DocsVectorsDir string
	// CodeVectorsDir is the embedded vector store's data directory for code
	// RAG. Separate from DocsVectorsDir because the indexes may use embedding
	// models with different dimensions.
	CodeVectorsDir string
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
	codeText *coderag.TextStore,
	reposDir, mergedPath string,
	docs DocsDeps,
) *Manager {
	m := &Manager{
		reg:            reg,
		git:            git,
		gfy:            gfy,
		engine:         engine,
		creds:          creds,
		codeText:       codeText,
		reposDir:       reposDir,
		mergedPath:     mergedPath,
		docsRootDir:    docs.DocsRootDir,
		docsVectorsDir: docs.DocsVectorsDir,
		codeVectorsDir: docs.CodeVectorsDir,
		docs: &docsBundle{
			codeRag: coderag.New(config.CodeRAG{}, nil, nil, engine, codeText),
		},
		baseCtx:  baseCtx,
		locks:    map[string]*sync.Mutex{},
		activity: map[string]string{},
	}
	m.leases = lease.New(m.TriggerRefresh)

	return m
}

// acquireDocs leases the active bundle until the returned release function is
// called. Configure waits for all leases before closing replaced stores, so an
// in-flight search/index can never race a live settings update.
func (m *Manager) acquireDocs() (*docsBundle, func()) {
	m.docsMu.RLock()

	return m.docs, m.docsMu.RUnlock
}

// Credentials exposes the credential store for API and MCP handlers.
func (m *Manager) Credentials() *credentials.Store { return m.creds }

// Wait blocks until in-flight background jobs finish.
func (m *Manager) Wait() { m.wg.Wait() }

// Close waits for background work and releases active vector stores. It is safe
// to call once server shutdown has stopped accepting new manager operations.
func (m *Manager) Close() error {
	m.lifecycleMu.Lock()
	if m.closing {
		m.lifecycleMu.Unlock()

		return nil
	}
	m.closing = true
	m.lifecycleMu.Unlock()

	// Wait for an in-flight Configure and prevent another one from starting
	// before the active bundle is detached.
	m.configureMu.Lock()
	defer m.configureMu.Unlock()

	m.Wait()

	m.docsMu.Lock()
	prev := m.docs
	m.docs = &docsBundle{}
	m.docsMu.Unlock()

	var errs []error
	if prev != nil && prev.store != nil {
		if err := prev.store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close docs vector store; %w", err))
		}
	}

	if prev != nil && prev.codeStore != nil {
		if err := prev.codeStore.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close code vector store; %w", err))
		}
	}

	return errors.Join(errs...)
}

// startWork registers one background task unless shutdown has started. The
// lifecycle lock prevents WaitGroup.Add from racing Close's Wait.
func (m *Manager) startWork() bool {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if m.closing {
		return false
	}

	m.wg.Add(1)

	return true
}

func (m *Manager) setActivity(id, step string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	m.activity[id] = step
}

func (m *Manager) clearActivity(id string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	delete(m.activity, id)
}

// Activity returns the pipeline step currently running for a repo ("sync",
// "graph", "docs", "docs_index", "code_index") or "" when idle.
func (m *Manager) Activity(id string) string {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	return m.activity[id]
}

// ReconcileInterruptedStages marks persisted running stages as failed. Activity
// is intentionally in-memory, so no work from a previous process can still be
// running when a new Manager starts.
func (m *Manager) ReconcileInterruptedStages(ctx context.Context) error {
	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, repo := range repos {
		changed := false
		for _, name := range generateOrder {
			st := repo.Stages.Get(name)
			if st == nil || st.Status != registry.StageRunning {
				continue
			}

			st.Status = registry.StageError
			st.Error = "interrupted by service restart"
			st.FinishedAt = time.Now()
			changed = true
		}
		if changed {
			if err := m.reg.Upsert(ctx, repo); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

// runStage executes one generation stage, persisting its running/ok/error state
// on the repo record and exposing it as the current activity while it runs.
func (m *Manager) runStage(ctx context.Context, repo *registry.Repo, name string, fn func() error) error {
	st := repo.Stages.Get(name)
	if st == nil {
		return fmt.Errorf("unknown stage %q", name)
	}

	m.setActivity(repo.ID, name)
	defer m.clearActivity(repo.ID)

	st.Status = registry.StageRunning
	st.Error = ""
	if err := m.reg.Upsert(ctx, repo); err != nil {
		slog.Error("save stage state", "repo", repo.ID, "stage", name, "error", err)
	}

	start := time.Now()
	err := fn()

	st.FinishedAt = time.Now()
	st.Commit = repo.LastCommit
	if err != nil {
		st.Status = registry.StageError
		st.Error = err.Error()

		slog.Error("stage failed", "repo", repo.ID, "stage", name, "error", err)
	} else {
		st.Status = registry.StageOK
		st.Error = ""

		slog.Info("stage finished", "repo", repo.ID, "stage", name,
			"took", time.Since(start).Round(time.Millisecond).String())
	}

	if uerr := m.reg.Upsert(ctx, repo); uerr != nil {
		slog.Error("save stage state", "repo", repo.ID, "stage", name, "error", uerr)
	}

	return err
}

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

// RemoveRepo deletes the record, local clone, generated docs and derived indexes.
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

	// Best-effort: drop the repo's vectors from the RAG indexes.
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.rag != nil {
		if err := d.rag.DeleteRepo(ctx, id); err != nil {
			slog.Error("delete repo from rag index", "repo", id, "error", err)
		}
	}

	if d.codeRag != nil {
		if err := d.codeRag.DeleteRepo(ctx, id); err != nil {
			slog.Error("delete repo from code index", "repo", id, "error", err)
		}
	}

	if err := m.removeRepoDocs(id); err != nil {
		return fmt.Errorf("remove generated docs for %s; %w", id, err)
	}

	if err := m.reg.Delete(ctx, id); err != nil {
		return err
	}

	if repo.Path != "" && filepath.HasPrefix(repo.Path, m.reposDir) {
		if err := os.RemoveAll(repo.Path); err != nil {
			return fmt.Errorf("remove clone %s; %w", repo.Path, err)
		}
	}

	if !m.startWork() {
		return nil
	}
	go func() {
		defer m.wg.Done()

		if err := m.rebuildMerged(m.baseCtx); err != nil {
			slog.Error("rebuild merged graph", "error", err)
		}
	}()

	return nil
}

// WarmCodeSearch creates missing bw FTS indexes for repositories that were
// tracked before normal code search was introduced. Existing indexes are left
// untouched; regular refreshes keep them current afterwards.
func (m *Manager) WarmCodeSearch(ctx context.Context) error {
	if m.codeText == nil {
		return nil
	}

	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.codeRag == nil {
		return nil
	}

	var errs []error
	for _, repo := range repos {
		if repo.Path == "" || !fileExists(filepath.Join(repo.Path, ".git")) {
			continue
		}

		hasIndex, err := m.codeText.HasRepo(ctx, repo.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("check code index for %s: %w", repo.ID, err))
			continue
		}
		if hasIndex {
			continue
		}

		if err := d.codeRag.IndexText(ctx, repo.ID, repo.Path); err != nil {
			errs = append(errs, fmt.Errorf("warm code index for %s: %w", repo.ID, err))
		}
	}

	return errors.Join(errs...)
}

// TriggerRefresh starts a background refresh for a repo. Concurrent triggers
// for the same repo serialize on the per-repo lock.
func (m *Manager) TriggerRefresh(id string) {
	if !m.startWork() {
		return
	}

	go func() {
		defer m.wg.Done()

		if err := m.Refresh(m.baseCtx, id); err != nil {
			slog.Error("refresh repo", "repo", id, "error", err)
		}
	}()
}

// generateOrder is the canonical stage execution order for selective runs:
// graph first (docs/code chunking can use it), then indexes and docs.
var generateOrder = []string{
	registry.StageGraph,
	registry.StageCodeIndex,
	registry.StageDocs,
	registry.StageDocsIndex,
}

// Generate runs only the selected generation stages for a repo, using the
// existing clone (no git sync). Valid targets: graph, docs, docs_index,
// code_index. Stage outcomes are recorded on the repo record; the returned
// error joins the failed stages.
func (m *Manager) Generate(ctx context.Context, id string, targets []string) error {
	want := map[string]bool{}
	for _, t := range targets {
		if !registry.ValidStage(t) {
			return fmt.Errorf("unknown generate target %q", t)
		}

		want[t] = true
	}

	if len(want) == 0 {
		return fmt.Errorf("no generate targets given")
	}

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

	if !fileExists(filepath.Join(repo.Path, ".git")) {
		return fmt.Errorf("repo %s has no clone yet; refresh it first", id)
	}

	defer m.clearActivity(id)

	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

	docsDir, docsDirErr := m.docsDirForRepo(repo)

	var errs []error

	for _, name := range generateOrder {
		if !want[name] {
			continue
		}

		var serr error

		switch name {
		case registry.StageGraph:
			serr = m.runStage(ctx, repo, name, func() error {
				if err := m.gfy.Update(ctx, repo.Path); err != nil {
					return err
				}

				repo.LastBuildAt = time.Now()

				if err := m.rebuildMerged(ctx); err != nil {
					slog.Error("rebuild merged graph", "error", err)
				}

				return nil
			})
		case registry.StageCodeIndex:
			serr = m.runStage(ctx, repo, name, func() error {
				if d.codeRag == nil {
					return fmt.Errorf("code index is not configured")
				}

				return d.codeRag.Index(ctx, repo.ID, repo.Path)
			})
		case registry.StageDocs:
			serr = m.runStage(ctx, repo, name, func() error {
				if d.gen == nil {
					return fmt.Errorf("docs generation disabled: enable docs and configure the LLM in settings")
				}

				if docsDirErr != nil {
					return docsDirErr
				}

				_, err := d.gen.Generate(ctx, repo.ID, repo.Path, docsDir)

				return err
			})

			// A failed docs run leaves no fresh markdown to index.
			if serr != nil && want[registry.StageDocsIndex] {
				want[registry.StageDocsIndex] = false
				errs = append(errs, fmt.Errorf("docs_index skipped: docs generation failed"))
			}
		case registry.StageDocsIndex:
			serr = m.runStage(ctx, repo, name, func() error {
				if d.rag == nil {
					return fmt.Errorf("docs index disabled: enable RAG and configure an embedder in settings")
				}

				if docsDirErr != nil {
					return docsDirErr
				}

				return d.rag.Index(ctx, repo.ID, docsDir)
			})
		}

		if serr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, serr))
		}
	}

	return errors.Join(errs...)
}

// TriggerGenerate starts a background selective generation for a repo.
func (m *Manager) TriggerGenerate(id string, targets []string) {
	if !m.startWork() {
		return
	}

	go func() {
		defer m.wg.Done()

		if err := m.Generate(m.baseCtx, id, targets); err != nil {
			slog.Error("generate", "repo", id, "targets", targets, "error", err)
		}
	}()
}

// TriggerReindexAll rebuilds optional docs/code indexes for every ready repo
// without fetching git or rebuilding graphify output. It is used after a live
// settings update because an ordinary refresh intentionally exits early when
// the repository commit has not changed.
func (m *Manager) TriggerReindexAll() {
	if !m.startWork() {
		return
	}

	go func() {
		defer m.wg.Done()

		repos, err := m.reg.List(m.baseCtx)
		if err != nil {
			slog.Error("list repos for reindex", "error", err)

			return
		}

		// Reindex repositories sequentially to avoid multiplying LLM/embedder
		// concurrency by the number of tracked repositories.
		for _, listed := range repos {
			if listed.Status != registry.StatusReady {
				continue
			}

			l := m.lock(listed.ID)
			l.Lock()

			repo, err := m.reg.Get(m.baseCtx, listed.ID)
			if err != nil {
				slog.Error("load repo for reindex", "repo", listed.ID, "error", err)
				l.Unlock()

				continue
			}

			if repo != nil && repo.Status == registry.StatusReady {
				m.buildDocsAndIndex(m.baseCtx, repo)
			}

			l.Unlock()
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

	m.setActivity(repo.ID, "sync")
	defer m.clearActivity(repo.ID)

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

	slog.Info("building graph", "repo", repo.ID, "path", repo.Path, "commit", shortSHA(repo.LastCommit))

	buildStart := time.Now()

	if err := m.runStage(ctx, repo, registry.StageGraph, func() error {
		return m.gfy.Update(ctx, repo.Path)
	}); err != nil {
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

// buildDocsAndIndex regenerates markdown docs and refreshes the RAG indexes
// (docs + code) for a repo. All steps are optional and best-effort: a nil
// generator/service or an error is logged and swallowed so the graph build
// result stands.
func (m *Manager) buildDocsAndIndex(ctx context.Context, repo *registry.Repo) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.gen == nil && d.rag == nil && d.codeRag == nil {
		return
	}

	// Code index first: it only needs the clone + graph, not the docs.
	if d.codeRag != nil {
		//nolint:errcheck // recorded on the stage state; never fails the build
		_ = m.runStage(ctx, repo, registry.StageCodeIndex, func() error {
			return d.codeRag.Index(ctx, repo.ID, repo.Path)
		})
	}

	if d.gen == nil && d.rag == nil {
		return
	}

	docsDir, err := m.docsDirForRepo(repo)
	if err != nil {
		slog.Error("resolve generated docs directory", "repo", repo.ID, "error", err)

		return
	}

	if d.gen != nil {
		if err := m.runStage(ctx, repo, registry.StageDocs, func() error {
			_, err := d.gen.Generate(ctx, repo.ID, repo.Path, docsDir)

			return err
		}); err != nil {
			return // no fresh docs -> skip indexing
		}
	}

	if d.rag != nil {
		//nolint:errcheck // recorded on the stage state; never fails the build
		_ = m.runStage(ctx, repo, registry.StageDocsIndex, func() error {
			return d.rag.Index(ctx, repo.ID, docsDir)
		})
	}
}

// MigrateDocs moves generated documentation persisted by older versions from
// each clone's krabby-docs directory into the external docs root. It performs
// filesystem moves/copies only and never invokes doc generation.
func (m *Manager) MigrateDocs(ctx context.Context) error {
	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, repo := range repos {
		if repo.Path == "" {
			continue
		}

		if _, err := m.docsDirForRepo(repo); err != nil {
			errs = append(errs, fmt.Errorf("migrate docs for %s: %w", repo.ID, err))
		}
	}

	return errors.Join(errs...)
}

func (m *Manager) docsDirForRepo(repo *registry.Repo) (string, error) {
	dir, rel, err := m.repoDocsPath(repo.ID)
	if err != nil {
		return "", err
	}

	legacy := filepath.Join(repo.Path, "krabby-docs")
	if err := migrateLegacyDocs(legacy, dir); err != nil {
		return "", fmt.Errorf("move %s to %s: %w", legacy, rel, err)
	}

	return dir, nil
}

func (m *Manager) repoDocsPath(repoID string) (dir, rel string, err error) {
	if m.docsRootDir == "" {
		return "", "", fmt.Errorf("docs root directory not configured")
	}

	rel = filepath.Clean(filepath.FromSlash(repoID))
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.ToSlash(rel) != repoID {
		return "", "", fmt.Errorf("invalid repository id %q", repoID)
	}

	return filepath.Join(m.docsRootDir, rel), rel, nil
}

func (m *Manager) removeRepoDocs(repoID string) error {
	_, rel, err := m.repoDocsPath(repoID)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(m.docsRootDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}
	defer root.Close()

	return root.RemoveAll(rel)
}

func migrateLegacyDocs(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}

	info, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("legacy docs path is not a directory")
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(src, dst); err == nil {
			slog.Info("migrated generated docs", "from", src, "to", dst)

			return nil
		}
	} else if err != nil {
		return err
	}

	if err := mergeLegacyDocs(src, dst); err != nil {
		return err
	}

	slog.Info("migrated generated docs", "from", src, "to", dst)

	return nil
}

// mergeLegacyDocs handles an existing destination and cross-device migrations.
// Existing destination files win; unique legacy files are retained.
func mergeLegacyDocs(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := mergeLegacyDocs(srcPath, dstPath); err != nil {
				return err
			}

			continue
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported legacy docs entry %s", srcPath)
		}

		if _, err := os.Stat(dstPath); err == nil {
			if err := os.Remove(srcPath); err != nil {
				return err
			}

			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if err := os.Rename(srcPath, dstPath); err != nil {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
			if err := os.Remove(srcPath); err != nil {
				return err
			}
		}
	}

	return os.Remove(src)
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
