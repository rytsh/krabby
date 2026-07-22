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
	"sort"
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
	mergeEnabled   bool
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

	// activity tracks the currently running pipeline steps per repo (transient,
	// in-memory): "sync" or registry.Stage* names. Multiple steps can run at
	// once (e.g. code_index in parallel with docs). Empty = idle.
	activityMu sync.Mutex
	activity   map[string]map[string]struct{}

	// stageMu serializes stage-state mutation + persistence on the shared repo
	// record so stages may run concurrently for the same repo.
	stageMu sync.Mutex

	// jobs tracks the cancel function of the currently running refresh/generate
	// job per repo so users can abort long builds manually.
	jobMu sync.Mutex
	jobs  map[string]*job

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
	mergeEnabled bool,
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
		mergeEnabled:   mergeEnabled,
		docsRootDir:    docs.DocsRootDir,
		docsVectorsDir: docs.DocsVectorsDir,
		codeVectorsDir: docs.CodeVectorsDir,
		docs: &docsBundle{
			codeRag: coderag.New(config.CodeRAG{}, nil, nil, engine, codeText),
		},
		baseCtx:  baseCtx,
		locks:    map[string]*sync.Mutex{},
		activity: map[string]map[string]struct{}{},
		jobs:     map[string]*job{},
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

	steps := m.activity[id]
	if steps == nil {
		steps = map[string]struct{}{}
		m.activity[id] = steps
	}

	steps[step] = struct{}{}
}

func (m *Manager) clearActivity(id, step string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	delete(m.activity[id], step)

	if len(m.activity[id]) == 0 {
		delete(m.activity, id)
	}
}

// clearRepoActivity removes all running-step markers for a repo. Used as a
// safety net when a whole pipeline run ends.
func (m *Manager) clearRepoActivity(id string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	delete(m.activity, id)
}

// Activity returns the pipeline steps currently running for a repo ("sync",
// "graph", "docs", "docs_index", "code_index"), comma-joined when several run
// in parallel, or "" when idle.
func (m *Manager) Activity(id string) string {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	steps := make([]string, 0, len(m.activity[id]))
	for step := range m.activity[id] {
		steps = append(steps, step)
	}

	sort.Strings(steps)

	return strings.Join(steps, ",")
}

// ActiveRepos returns the repos that currently have running pipeline steps,
// mapping each repo id to its comma-joined steps. This lets the UI show live
// jobs without scanning every tracked repo.
func (m *Manager) ActiveRepos() map[string]string {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	out := make(map[string]string, len(m.activity))
	for id, stepSet := range m.activity {
		steps := make([]string, 0, len(stepSet))
		for step := range stepSet {
			steps = append(steps, step)
		}
		sort.Strings(steps)
		out[id] = strings.Join(steps, ",")
	}

	return out
}

// job is the cancellation handle of one running refresh/generate run.
type job struct {
	cancel context.CancelFunc
}

// registerJob derives a cancellable context for a repo's running job and
// registers its handle so CancelJob can abort it. The returned cleanup must be
// called when the job finishes; it releases the context and unregisters the
// handle (unless a newer job already replaced it). Per-repo jobs serialize on
// the repo lock, so at most one handle exists per id.
func (m *Manager) registerJob(ctx context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	j := &job{cancel: cancel}

	m.jobMu.Lock()
	m.jobs[id] = j
	m.jobMu.Unlock()

	return ctx, func() {
		m.jobMu.Lock()
		if m.jobs[id] == j {
			delete(m.jobs, id)
		}
		m.jobMu.Unlock()

		cancel()
	}
}

// CancelJob aborts the refresh/generate job currently running for a repo by
// cancelling its context; the in-flight git/graphify subprocess is killed and
// the outcome is recorded as cancelled. It reports whether a job was running.
func (m *Manager) CancelJob(id string) bool {
	m.jobMu.Lock()
	j := m.jobs[id]
	m.jobMu.Unlock()

	if j == nil {
		return false
	}

	slog.Info("cancelling running job", "repo", id)
	j.cancel()

	return true
}

// ErrCancelled is recorded as the repo's LastError when a running job is
// aborted via CancelJob.
var ErrCancelled = errors.New("cancelled by user")

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

// BackfillGraphIgnore rebuilds the graph for every tracked repo whose clone is
// missing the krabby-managed .graphifyignore block. Repos tracked before the
// ignore feature keep their pre-existing graph (which still contains testdata /
// fixture nodes) until something triggers a rebuild; this runs that rebuild once
// on startup so those stale nodes are pruned. Work happens in the background and
// never blocks startup.
func (m *Manager) BackfillGraphIgnore(ctx context.Context) {
	repos, err := m.reg.List(ctx)
	if err != nil {
		slog.Error("list repos for graphignore backfill", "error", err)

		return
	}

	var stale []string
	for _, repo := range repos {
		if repo.Path == "" || !fileExists(filepath.Join(repo.Path, ".git")) {
			continue
		}

		// A rebuild only helps once a graph already exists; brand-new repos get
		// the ignore file on their first build anyway.
		if !fileExists(graphify.GraphPath(repo.Path)) {
			continue
		}

		if graphify.HasManagedIgnore(repo.Path) {
			continue
		}

		stale = append(stale, repo.ID)
	}

	if len(stale) == 0 {
		return
	}

	slog.Info("backfilling .graphifyignore and rebuilding graphs", "repos", stale)

	for _, id := range stale {
		m.TriggerGenerate(id, []string{registry.StageGraph})
	}
}

// CleanupMergedGraph removes a stale merged graph file left over from a run that
// had cross-repo merging enabled. It is a no-op when merging is enabled or no
// file exists. Prevents serving an outdated merged graph after merge is turned
// off.
func (m *Manager) CleanupMergedGraph() {
	if m.mergeEnabled || m.mergedPath == "" {
		return
	}

	if err := os.Remove(m.mergedPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("remove stale merged graph", "path", m.mergedPath, "error", err)
	}
}

// runStage executes one generation stage, persisting its running/ok/error state
// on the repo record and exposing it as a current activity while it runs.
// Stage-state mutation and persistence are serialized on stageMu so multiple
// stages of the same repo may run concurrently.
func (m *Manager) runStage(ctx context.Context, repo *registry.Repo, name string, fn func() error) error {
	m.stageMu.Lock()
	st := repo.Stages.Get(name)
	if st == nil {
		m.stageMu.Unlock()

		return fmt.Errorf("unknown stage %q", name)
	}

	m.setActivity(repo.ID, name)
	defer m.clearActivity(repo.ID, name)

	st.Status = registry.StageRunning
	st.Error = ""
	if err := m.reg.Upsert(ctx, repo); err != nil {
		slog.Error("save stage state", "repo", repo.ID, "stage", name, "error", err)
	}
	m.stageMu.Unlock()

	start := time.Now()
	err := fn()

	m.stageMu.Lock()
	defer m.stageMu.Unlock()

	st.FinishedAt = time.Now()
	st.Commit = repo.LastCommit
	if err != nil {
		// A cancelled job context reads better as "cancelled by user" than the
		// raw "context canceled" / "signal: killed" subprocess errors.
		if ctx.Err() != nil {
			err = ErrCancelled
		}

		st.Status = registry.StageError
		st.Error = err.Error()

		slog.Error("stage failed", "repo", repo.ID, "stage", name, "error", err)
	} else {
		st.Status = registry.StageOK
		st.Error = ""

		slog.Info("stage finished", "repo", repo.ID, "stage", name,
			"took", time.Since(start).Round(time.Millisecond).String())
	}

	// Persist even when the job context was cancelled mid-stage.
	if uerr := m.reg.Upsert(context.WithoutCancel(ctx), repo); uerr != nil {
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

// AddRepoWait registers a repository, starts the clone+build in the background
// and waits for it to finish for as long as ctx allows. The build itself runs
// detached from ctx, so a caller cancel or timeout never aborts it: when the
// wait ends early the current in-progress record is returned with done=false
// and the build keeps running (poll repo_status for the final state).
//
// A build failure is reported through the returned record's Status ("error") and
// LastError, not as a Go error, so callers always get the final state back.
func (m *Manager) AddRepoWait(ctx context.Context, url, branch string) (*registry.Repo, bool, error) {
	id, _, _, err := m.registerRepo(ctx, url, branch)
	if err != nil {
		return nil, false, err
	}

	return m.RefreshWait(ctx, id)
}

// RefreshWait pulls and rebuilds a repository in the background and waits until
// the run finishes or ctx is done, then returns the latest repo record. done
// reports whether the refresh completed within the wait; when false the build
// continues detached and the record reflects the in-progress state.
// A build failure is surfaced via the record's Status/LastError rather than a
// Go error; only unexpected lookup failures return an error.
func (m *Manager) RefreshWait(ctx context.Context, id string) (*registry.Repo, bool, error) {
	// The refresh runs on the manager lifecycle context: a client that stops
	// waiting (client-side tool timeout, ctrl+c, MCP cancellation) must not
	// kill the clone/build mid-flight.
	finished := m.refreshAsync(id)

	done := false
	select {
	case <-finished:
		done = true
	case <-ctx.Done():
	}

	// Read the record even after a caller cancel so the in-progress (or
	// terminal) state is always returned.
	repo, err := m.reg.Get(context.WithoutCancel(ctx), id)
	if err != nil {
		return nil, done, err
	}

	if repo == nil {
		return nil, done, fmt.Errorf("repo %s not found", id)
	}

	return repo, done, nil
}

// refreshAsync starts a refresh on the manager lifecycle context and returns a
// channel closed when that refresh attempt completes. Refresh persists
// StatusError + LastError on failure, so callers read the terminal state back
// from the registry. During shutdown the channel is closed immediately.
func (m *Manager) refreshAsync(id string) <-chan struct{} {
	finished := make(chan struct{})
	if !m.startWork() {
		close(finished)

		return finished
	}

	go func() {
		defer m.wg.Done()
		defer close(finished)

		if err := m.Refresh(m.baseCtx, id); err != nil {
			slog.Error("refresh repo", "repo", id, "error", err)
		}
	}()

	return finished
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

// stageDeps declares which stages each stage depends on. A stage's dependency
// is auto-added to a selective run only when the dependency's output does not
// already exist (see stageOutputExists), so asking for e.g. docs_index alone
// stays cheap when docs are already generated but never produces an empty index
// when they are not. Dependencies are transitive: docs_index -> docs -> graph.
var stageDeps = map[string][]string{
	registry.StageCodeIndex: {registry.StageGraph},
	registry.StageDocs:      {registry.StageGraph},
	registry.StageDocsIndex: {registry.StageDocs},
}

// stageOutputExists reports whether the persisted output a stage would produce
// is already present on disk, so a dependency need not be rebuilt. docsDir may
// be empty when the docs directory could not be resolved, in which case docs
// are treated as absent.
func (m *Manager) stageOutputExists(name string, repo *registry.Repo, docsDir string) bool {
	switch name {
	case registry.StageGraph:
		return fileExists(graphify.GraphPath(repo.Path))
	case registry.StageDocs:
		return docsDir != "" && dirHasMarkdown(docsDir)
	default:
		// code_index and docs_index are consumed only as targets, never as a
		// dependency of another stage, so their presence never gates auto-add.
		return false
	}
}

// resolveStageDeps expands want in place so that every requested stage has its
// missing prerequisites scheduled too. A prerequisite is added only when its
// output does not already exist; existing outputs are reused. Newly added
// prerequisites are themselves resolved, so the dependency chain is followed
// transitively (docs_index pulls in docs, which pulls in graph).
func (m *Manager) resolveStageDeps(want map[string]bool, repo *registry.Repo, docsDir, id string) {
	// generateOrder is dependency-topological (deps precede dependents), so a
	// reverse walk lets a dependent enable its dependency before we reach it.
	for i := len(generateOrder) - 1; i >= 0; i-- {
		name := generateOrder[i]
		if !want[name] {
			continue
		}

		for _, dep := range stageDeps[name] {
			if want[dep] || m.stageOutputExists(dep, repo, docsDir) {
				continue
			}

			slog.Info("stage dependency missing; scheduling it first",
				"repo", id, "stage", name, "dependency", dep)
			want[dep] = true
		}
	}
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

	docsDir, docsDirErr := m.docsDirForRepo(repo)

	// Auto-schedule any missing prerequisites of the requested stages. Each
	// stage declares its dependencies in stageDeps and they are only added when
	// their output is absent, so downstream stages never fall back to degraded
	// output (e.g. graph-less docs) or index nothing (docs_index with no docs),
	// while a request whose prerequisites already exist stays cheap. The chain
	// is followed transitively: docs_index -> docs -> graph.
	resolveDocsDir := docsDir
	if docsDirErr != nil {
		resolveDocsDir = ""
	}
	m.resolveStageDeps(want, repo, resolveDocsDir, id)

	// Run the stages on a cancellable job context so CancelJob can abort them.
	var finish func()
	ctx, finish = m.registerJob(ctx, id)
	defer finish()

	defer m.clearRepoActivity(id)

	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

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

			// The graph underpins symbol-aware code chunking and docs
			// generation. If it failed there is no graph to build on, so skip
			// the dependent stages rather than emit graph-less output.
			if serr != nil {
				for _, dep := range []string{registry.StageCodeIndex, registry.StageDocs, registry.StageDocsIndex} {
					if want[dep] {
						want[dep] = false
						errs = append(errs, fmt.Errorf("%s skipped: graph build failed", dep))
					}
				}
			}
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

// GenerateWait runs the selected generation stages for a repo in the background
// and waits until the run finishes or ctx is done, then returns the latest repo
// record. done reports whether the generation completed within the wait; when
// false the run continues detached and the record reflects the in-progress
// state. Stage failures are surfaced via the record's Status/LastError rather
// than a Go error; only unexpected lookup failures return an error.
func (m *Manager) GenerateWait(ctx context.Context, id string, targets []string) (*registry.Repo, bool, error) {
	// The generation runs on the manager lifecycle context: a client that stops
	// waiting (client-side tool timeout, ctrl+c, MCP cancellation) must not
	// kill the build mid-flight.
	finished := m.generateAsync(id, targets)

	done := false
	select {
	case <-finished:
		done = true
	case <-ctx.Done():
	}

	// Read the record even after a caller cancel so the in-progress (or
	// terminal) state is always returned.
	repo, err := m.reg.Get(context.WithoutCancel(ctx), id)
	if err != nil {
		return nil, done, err
	}

	if repo == nil {
		return nil, done, fmt.Errorf("repo %s not found", id)
	}

	return repo, done, nil
}

// generateAsync starts a selective generation on the manager lifecycle context
// and returns a channel closed when that run completes. Generate persists
// StatusError + LastError on failure, so callers read the terminal state back
// from the registry. During shutdown the channel is closed immediately.
func (m *Manager) generateAsync(id string, targets []string) <-chan struct{} {
	finished := make(chan struct{})
	if !m.startWork() {
		close(finished)

		return finished
	}

	go func() {
		defer m.wg.Done()
		defer close(finished)

		if err := m.Generate(m.baseCtx, id, targets); err != nil {
			slog.Error("generate", "repo", id, "targets", targets, "error", err)
		}
	}()

	return finished
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

	jobCtx, finish := m.registerJob(ctx, id)
	defer finish()

	if err := m.refresh(jobCtx, repo); err != nil {
		// A manual CancelJob cancels jobCtx while the parent stays alive;
		// record that distinctly from a real failure or a shutdown.
		if jobCtx.Err() != nil && ctx.Err() == nil {
			err = ErrCancelled
		}

		repo.Status = registry.StatusError
		repo.LastError = err.Error()

		// Persist even when the job context was cancelled.
		if uerr := m.reg.Upsert(context.WithoutCancel(ctx), repo); uerr != nil {
			slog.Error("save repo error state", "repo", id, "error", uerr)
		}

		return err
	}

	return nil
}

func (m *Manager) refresh(ctx context.Context, repo *registry.Repo) error {
	slog.Info("working on repo", "repo", repo.ID, "commit", shortSHA(repo.LastCommit))

	defer m.clearRepoActivity(repo.ID)

	graphPath := graphify.GraphPath(repo.Path)
	hadGraph := fileExists(graphPath)

	m.setActivity(repo.ID, "sync")
	changed, err := m.sync(ctx, repo)
	m.clearActivity(repo.ID, "sync")

	if err != nil {
		return err
	}

	repo.LastSyncAt = time.Now()

	// A stale graph built before the current .graphifyignore rules still holds
	// excluded nodes (testdata, fixtures, ...). Rebuild it even when git is
	// unchanged so a manual refresh actually cleans it; gfy.Update writes the
	// ignore file and force-rebuilds.
	staleIgnore := hadGraph && m.gfy.GraphNeedsIgnoreRebuild(repo.Path)

	if hadGraph && !changed && !staleIgnore {
		// Nothing new; keep current status.
		repo.Status = registry.StatusReady
		repo.LastError = ""

		slog.Info("repo already up to date, graph unchanged", "repo", repo.ID, "commit", shortSHA(repo.LastCommit))

		return m.reg.Upsert(ctx, repo)
	}

	if staleIgnore && !changed {
		slog.Info("graph contains now-excluded files; rebuilding to apply ignore rules", "repo", repo.ID)
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

	// code_index and docs have no data dependency (both need only the clone +
	// graph) and hit different backends (embedder vs LLM), so they run in
	// parallel; runStage serializes their stage-state writes on stageMu.
	var wg sync.WaitGroup
	if d.codeRag != nil {
		// The delta must be captured before runStage mutates the stage state.
		changed, incremental := m.codeIndexDelta(ctx, repo)

		wg.Add(1)
		go func() {
			defer wg.Done()

			//nolint:errcheck // recorded on the stage state; never fails the build
			_ = m.runStage(ctx, repo, registry.StageCodeIndex, func() error {
				if incremental {
					return d.codeRag.IndexChanged(ctx, repo.ID, repo.Path, changed)
				}

				return d.codeRag.Index(ctx, repo.ID, repo.Path)
			})
		}()
	}
	defer wg.Wait()

	if d.gen == nil && d.rag == nil {
		return
	}

	docsDir, err := m.docsDirForRepo(repo)
	if err != nil {
		slog.Error("resolve generated docs directory", "repo", repo.ID, "error", err)

		return
	}

	docsChanged := true
	if d.gen != nil {
		var man *docgen.Manifest
		if err := m.runStage(ctx, repo, registry.StageDocs, func() error {
			var gerr error
			man, gerr = d.gen.Generate(ctx, repo.ID, repo.Path, docsDir)

			return gerr
		}); err != nil {
			return // no fresh docs -> skip indexing
		}

		if man != nil {
			docsChanged = man.ChangedDocs
		}
	}

	if d.rag == nil {
		return
	}

	// Unchanged documentation with a previously successful index needs no
	// re-embedding — but only when the index actually still holds this
	// repo. A stage marked OK can outlive its vectors (e.g. the docs were
	// regenerated by a run that did not re-embed them, or the index was
	// cleared), which silently drops the repo from docs search. Verify the
	// index has the repo before trusting the skip.
	if st := repo.Stages.Get(registry.StageDocsIndex); !docsChanged && st.Status == registry.StageOK {
		has, err := d.rag.HasRepo(ctx, repo.ID)
		if err != nil {
			slog.Warn("docs index presence check failed; reindexing", "repo", repo.ID, "error", err)
		} else if has {
			slog.Info("documentation unchanged, skipping docs index", "repo", repo.ID)

			return
		} else {
			slog.Info("docs index missing for repo despite ok stage; reindexing", "repo", repo.ID)
		}
	}

	//nolint:errcheck // recorded on the stage state; never fails the build
	_ = m.runStage(ctx, repo, registry.StageDocsIndex, func() error {
		return d.rag.Index(ctx, repo.ID, docsDir)
	})
}

// codeIndexDelta decides whether the code index can be updated incrementally:
// the previous code_index run must have succeeded at a known commit that still
// exists, the FTS index must actually hold the repo, and git must be able to
// name the files changed since. It returns the changed paths (possibly empty:
// a no-op update) and whether the incremental path applies; on false the caller
// performs a full rebuild.
func (m *Manager) codeIndexDelta(ctx context.Context, repo *registry.Repo) ([]string, bool) {
	st := repo.Stages.Get(registry.StageCodeIndex)
	if st == nil || st.Status != registry.StageOK || st.Commit == "" || repo.LastCommit == "" {
		return nil, false
	}

	if m.codeText != nil {
		if has, err := m.codeText.HasRepo(ctx, repo.ID); err != nil || !has {
			return nil, false
		}
	}

	if st.Commit == repo.LastCommit {
		return nil, true // already indexed at this commit: nothing to do
	}

	changed, err := m.git.DiffNames(ctx, repo.Path, st.Commit, repo.LastCommit)
	if err != nil {
		slog.Warn("code index: diff failed, falling back to full reindex",
			"repo", repo.ID, "from", shortSHA(st.Commit), "to", shortSHA(repo.LastCommit), "error", err)

		return nil, false
	}

	return changed, true
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

// MigrateRepoIDs re-keys repositories tracked under legacy truncated ids (the
// last two URL path segments) to the full-path ids ParseRepoID now derives
// from the stored git URL (nested groups included). The clone and generated
// docs move to the new id's directories; vectors and text-search entries
// indexed under the old id are dropped and the index stages reset so the next
// refresh rebuilds them under the new id.
func (m *Manager) MigrateRepoIDs(ctx context.Context) error {
	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, repo := range repos {
		newID, perr := gitops.ParseRepoID(repo.URL)
		if perr != nil || newID == repo.ID {
			continue
		}

		if err := m.migrateRepoID(ctx, repo, newID); err != nil {
			errs = append(errs, fmt.Errorf("migrate repo %s to %s: %w", repo.ID, newID, err))
		}
	}

	return errors.Join(errs...)
}

func (m *Manager) migrateRepoID(ctx context.Context, repo *registry.Repo, newID string) error {
	oldID := repo.ID

	if existing, err := m.reg.Get(ctx, newID); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("target id already tracked")
	}

	// Move the clone when it lives inside the managed repos directory.
	newPath := filepath.Join(m.reposDir, filepath.FromSlash(newID))
	switch {
	case repo.Path == "":
		repo.Path = newPath
	case repo.Path != newPath && filepath.HasPrefix(repo.Path, m.reposDir):
		if err := moveDir(repo.Path, newPath); err != nil {
			return fmt.Errorf("move clone; %w", err)
		}

		repo.Path = newPath
	}

	// Move the generated docs to the new id's directory.
	if m.docsRootDir != "" {
		oldDocs, _, oerr := m.repoDocsPath(oldID)
		newDocs, _, nerr := m.repoDocsPath(newID)
		if oerr == nil && nerr == nil {
			if err := moveDir(oldDocs, newDocs); err != nil {
				return fmt.Errorf("move generated docs; %w", err)
			}
		}
	}

	// Indexes key their entries by repo id: drop everything stored under the
	// old id and reset the index stages so the next refresh (or the startup
	// warm-up for text search) rebuilds them under the new id.
	d, release := m.acquireDocs()
	if d.rag != nil {
		if err := d.rag.DeleteRepo(ctx, oldID); err != nil {
			slog.Warn("drop docs vectors for legacy id", "repo", oldID, "error", err)
		}
	}
	if d.codeRag != nil {
		if err := d.codeRag.DeleteRepo(ctx, oldID); err != nil {
			slog.Warn("drop code vectors for legacy id", "repo", oldID, "error", err)
		}
	}
	release()

	if m.codeText != nil {
		if err := m.codeText.DeleteRepo(ctx, oldID); err != nil {
			slog.Warn("drop text search entries for legacy id", "repo", oldID, "error", err)
		}
	}

	repo.Stages.DocsIndex = registry.StageState{}
	repo.Stages.CodeIndex = registry.StageState{}

	repo.ID = newID
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return err
	}

	if err := m.reg.Delete(ctx, oldID); err != nil {
		return err
	}

	slog.Info("migrated repo to full-path id", "from", oldID, "to", newID)

	return nil
}

// moveDir renames src onto dst (creating dst's parent). A missing src is a
// no-op; an existing dst is an error so migrations never clobber data.
func moveDir(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}

	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination %s already exists", dst)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	return os.Rename(src, dst)
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
// It is a no-op when cross-repo merging is disabled (the default): independent
// repos produce no cross-repo edges, so the merged graph is just a costly
// disjoint union.
func (m *Manager) rebuildMerged(ctx context.Context) error {
	if !m.mergeEnabled {
		return nil
	}

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
		if !m.mergeEnabled {
			return "", fmt.Errorf("cross-repo merged graph is disabled; pass a repo id (enable it with graphify.merge)")
		}

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

// MergedPath returns the merged cross-repo graph location ("" when merging is
// disabled or the graph is not built yet).
func (m *Manager) MergedPath() string {
	if !m.mergeEnabled || !fileExists(m.mergedPath) {
		return ""
	}

	return m.mergedPath
}

func fileExists(p string) bool {
	_, err := os.Stat(p)

	return err == nil
}

// dirHasMarkdown reports whether dir contains at least one generated markdown
// document, used to decide whether the docs stage has already produced output.
func dirHasMarkdown(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			return true
		}
	}

	return false
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
