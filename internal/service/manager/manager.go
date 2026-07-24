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
	"strconv"
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
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/taskstore"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
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
	sourcesRootDir string

	// Web content sources (wiki pages, Confluence spaces). webFetchers maps a
	// collection type to its fetcher implementation; new source types register
	// here (see SetWebSources).
	webStore    *websource.Store
	webFetchers map[string]websource.Fetcher

	// queue is the central bounded work queue. Every background task (repo
	// refresh/generate, web-source sync, reindex) is submitted here so a single
	// configurable concurrency limit governs how many run at once, instead of
	// each trigger spawning its own unbounded goroutine.
	queue *queue.Queue

	// taskStore persists queued/running tasks so the backlog survives a
	// restart. Set via SetTaskStore before RestoreTasks; nil disables
	// persistence (tests, or when the store failed to open).
	taskStore TaskStore

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

	// progress tracks live counters for a long-running step per id (transient,
	// in-memory), so the UI can show "1200/4634 embedded, ~26%". Keyed by id
	// (repo id or web-source scope key). Cleared when the step ends.
	progressMu sync.Mutex
	progress   map[string]Progress

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

	// codeWarm tracks background warming of the normal (full-text) code search
	// index. pending holds repo ids whose index has not been built yet; a
	// per-repo mutex (inflight) serializes on-demand warming so a search that
	// races the background pass never returns partial results. See
	// WarmCodeSearch and ensureCodeIndex.
	codeWarmMu sync.Mutex
	codeWarm   map[string]*sync.Mutex
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
	// SourcesRootDir stores synced web-source markdown by collection name.
	SourcesRootDir string
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
		sourcesRootDir: docs.SourcesRootDir,
		docs: &docsBundle{
			codeRag: coderag.New(config.CodeRAG{}, nil, nil, engine, codeText),
		},
		baseCtx:  baseCtx,
		locks:    map[string]*sync.Mutex{},
		activity: map[string]map[string]struct{}{},
		progress: map[string]Progress{},
		jobs:     map[string]*job{},
		codeWarm: map[string]*sync.Mutex{},
	}
	// The queue's limit is updated from persisted settings at startup and on
	// every settings change (see SetTaskConcurrency); it starts at the default.
	m.queue = queue.New(baseCtx, queue.DefaultConcurrency)
	return m
}

// Background task kinds submitted to the central queue. They classify work in
// the Activity UI and namespace the dedup keys.
const (
	taskKindRefresh  = "refresh"
	taskKindGenerate = "generate"
	taskKindWebSync  = "websync"
	taskKindReindex  = "reindex"
)

// SetTaskConcurrency updates the central work queue's concurrency limit live.
// A value <= 0 falls back to the queue default. Called at startup and whenever
// the setting changes in the UI/REST/MCP.
func (m *Manager) SetTaskConcurrency(n int) {
	m.queue.SetLimit(n)
}

// TaskSnapshot returns the current queue state (limit, running/queued counts,
// and queued/running/recent tasks) for the Activity UI.
func (m *Manager) TaskSnapshot() queue.Snapshot {
	return m.queue.Snapshot()
}

// BumpTask moves the queued task with the given seq to the front of the backlog
// so it starts next when a slot frees. It reports whether a matching queued
// task was found (a running or finished task cannot be bumped).
func (m *Manager) BumpTask(seq uint64) bool {
	return m.queue.Bump(seq)
}

// CancelTask cancels the task with the given seq. A queued task is dropped from
// the backlog; a running task has its underlying job aborted (its context is
// cancelled, killing the in-flight git/graphify/index work). It reports whether
// a matching task was found in either state.
func (m *Manager) CancelTask(seq uint64) bool {
	if m.queue.CancelSeq(seq) {
		return true
	}

	// Not queued: it may be the task currently running. Translate the per-task
	// cancel into cancelling that task's job context via its repo/scope id.
	if id, ok := m.queue.RunningID(seq); ok {
		return m.CancelJob(id)
	}

	return false
}

// CancelPendingForRepo drops every queued (not-yet-started) task for a repo id
// and returns how many were removed. Running work is left alone.
func (m *Manager) CancelPendingForRepo(id string) int {
	return m.queue.CancelPending(id)
}

// TaskStore persists queued/running tasks so the work queue survives a restart.
// It is the queue.Persister plus a List used to replay records on startup.
type TaskStore interface {
	queue.Persister
	List(ctx context.Context) ([]taskstore.PersistedTask, error)
}

// SetTaskStore installs the durable task store and wires it into the queue as
// the persister. Call it once at startup, before RestoreTasks and before any
// trigger enqueues work, so every submit is recorded.
func (m *Manager) SetTaskStore(store TaskStore) {
	m.taskStore = store
	m.queue.SetPersister(store)
}

// RestoreTasks re-enqueues tasks that were queued (or running) when the process
// last stopped. Records are read in FIFO seq order and rebuilt from their spec;
// a task that was running before the restart comes back as queued (its previous
// run died with the process). Unknown or malformed specs are dropped so a bad
// record cannot wedge startup. It is a no-op when no store is configured.
func (m *Manager) RestoreTasks(ctx context.Context) error {
	if m.taskStore == nil {
		return nil
	}

	tasks, err := m.taskStore.List(ctx)
	if err != nil {
		return err
	}

	restored := 0
	for _, pt := range tasks {
		t, ok := m.rebuildTask(pt.Spec)
		if !ok {
			// Drop records we can no longer interpret so they are not retried
			// on every restart.
			m.taskStore.Remove(pt.Seq)

			continue
		}

		m.queue.Restore(pt.Seq, t)
		restored++
	}

	if restored > 0 {
		slog.Info("restored persisted background tasks", "count", restored)
	}

	return nil
}

// rebuildTask reconstructs a queue.Task (including its Run closure) from a
// persisted spec. It reports ok=false for specs whose target no longer exists
// or whose kind is unknown, so the caller can drop the record.
func (m *Manager) rebuildTask(spec queue.Spec) (queue.Task, bool) {
	switch spec.Kind {
	case taskKindRefresh:
		return m.refreshTask(spec.ID), true

	case taskKindGenerate:
		targets := splitTargets(spec.Params["targets"])
		if len(targets) == 0 {
			return queue.Task{}, false
		}
		force := spec.Params["force"] == "true"

		return m.generateTask(spec.ID, targets, force), true

	case taskKindWebSync:
		// Persisted websync IDs are the scope key ("web:<name>"); recover the
		// collection name for the rebuilt closure.
		name := websource.CollectionName(spec.ID)
		if name == "" {
			return queue.Task{}, false
		}

		return m.webSyncTask(name), true

	case taskKindReindex:
		if spec.ID == "*" {
			return m.reindexAllTask(), true
		}

		return m.reindexTask(spec.ID), true

	default:
		return queue.Task{}, false
	}
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

	// Stop accepting queued work and drain running tasks, then wait for the
	// few remaining raw goroutines (e.g. merged-graph rebuild after a delete).
	m.queue.Close()
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

// Progress is a live view of a long-running step's counters, so the UI can show
// a determinate progress bar. Phase names the current work ("fetch",
// "index"); Done/Total are its item counters (e.g. embedded chunks). All fields
// are transient and reset when the step ends.
type Progress struct {
	Phase string `json:"phase"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// setProgress records the live counters for id's current step, replacing any
// prior value. A zero Total means "indeterminate" (the UI shows a spinner).
func (m *Manager) setProgress(id string, p Progress) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()

	m.progress[id] = p
}

// clearProgress removes id's progress counters (step finished or aborted).
func (m *Manager) clearProgress(id string) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()

	delete(m.progress, id)
}

// Progress returns id's live step counters and whether any are set.
func (m *Manager) Progress(id string) (Progress, bool) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()

	p, ok := m.progress[id]

	return p, ok
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
	// Drop any not-yet-started tasks for this repo from the queue so a backlog
	// can be cleared, then cancel the running job (if any).
	dropped := m.queue.CancelPending(id)

	m.jobMu.Lock()
	j := m.jobs[id]
	m.jobMu.Unlock()

	if j == nil {
		return dropped > 0
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
		m.TriggerGenerate(id, []string{registry.StageGraph}, false)
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

	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	if l, ok := m.locks[id]; ok {
		return l
	}

	l := &sync.Mutex{}
	m.locks[id] = l

	return l
}

// AddRepo registers a repository and starts a background clone+build.
// If the repo already exists, it just triggers a refresh. namespace assigns the
// new repo to a namespace ("" == default); it is ignored for an existing repo
// (use SetRepoNamespace to move one).
func (m *Manager) AddRepo(ctx context.Context, url, branch, namespace string) (*registry.Repo, error) {
	id, repo, _, err := m.registerRepo(ctx, url, branch, namespace)
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
func (m *Manager) AddRepoWait(ctx context.Context, url, branch, namespace string) (*registry.Repo, bool, error) {
	id, _, _, err := m.registerRepo(ctx, url, branch, namespace)
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

// refreshTask builds the queue task for a repo refresh, including its Spec so
// the work is persisted and can be rebuilt after a restart.
func (m *Manager) refreshTask(id string) queue.Task {
	return queue.Task{
		ID:    id,
		Kind:  taskKindRefresh,
		Title: "Refresh " + id,
		Key:   taskKindRefresh + ":" + id,
		Spec:  queue.Spec{Kind: taskKindRefresh, ID: id},
		Run: func(ctx context.Context) error {
			if err := m.Refresh(ctx, id); err != nil {
				slog.Error("refresh repo", "repo", id, "error", err)

				return err
			}

			return nil
		},
	}
}

// submitRefresh enqueues a repo refresh on the central work queue and returns
// its handle. Concurrent refreshes for the same repo coalesce onto one queued
// task (queue dedup), and the queue bounds how many refreshes run at once.
func (m *Manager) submitRefresh(id string) *queue.Handle {
	return m.queue.Submit(m.refreshTask(id))
}

// refreshAsync enqueues a refresh and returns a channel closed when that
// refresh attempt completes. Refresh persists StatusError + LastError on
// failure, so callers read the terminal state back from the registry. During
// shutdown the channel is closed immediately.
func (m *Manager) refreshAsync(id string) <-chan struct{} {
	return m.submitRefresh(id).Done()
}

// registerRepo parses the url, upserts a pending record if the repo is new, and
// reports whether it already existed. It performs no clone/build itself.
func (m *Manager) registerRepo(ctx context.Context, url, branch, namespace string) (id string, repo *registry.Repo, existed bool, err error) {
	id, err = gitops.ParseRepoID(url)
	if err != nil {
		return "", nil, false, err
	}

	if strings.TrimSpace(namespace) == registry.NamespaceAll {
		return "", nil, false, fmt.Errorf("namespace %q is reserved", registry.NamespaceAll)
	}

	if existing, gerr := m.reg.Get(ctx, id); gerr != nil {
		return "", nil, false, gerr
	} else if existing != nil {
		return id, existing, true, nil
	}

	repo = &registry.Repo{
		ID:        id,
		URL:       url,
		Branch:    branch,
		Path:      filepath.Join(m.reposDir, filepath.FromSlash(id)),
		Status:    registry.StatusPending,
		Namespace: registry.NormalizeNamespace(namespace),
	}
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return "", nil, false, err
	}

	return id, repo, false, nil
}

// SetRepoNamespace moves a tracked repo into a namespace. ref is resolved the
// same way as other repo refs (exact id or unique suffix). Passing an empty
// namespace (or "default") returns the repo to the default bucket; NamespaceAll
// is rejected. The change does not touch the pipeline, so no rebuild is needed.
func (m *Manager) SetRepoNamespace(ctx context.Context, ref, namespace string) (*registry.Repo, error) {
	repo, err := m.reg.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("repo %s not found", ref)
	}

	l := m.lock(repo.ID)
	l.Lock()
	defer l.Unlock()

	return m.reg.SetNamespace(ctx, repo.ID, namespace)
}

// UpsertNamespace creates or updates the description metadata for a namespace.
func (m *Manager) UpsertNamespace(ctx context.Context, name, description string) (*registry.NamespaceRecord, error) {
	return m.reg.UpsertNamespace(ctx, name, description)
}

// DeleteNamespace removes a namespace's description record. Repos keep their tag.
func (m *Manager) DeleteNamespace(ctx context.Context, name string) error {
	return m.reg.DeleteNamespace(ctx, name)
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

	snapshotRoot := m.snapshotRoot(id)
	if err := os.RemoveAll(snapshotRoot); err != nil {
		return fmt.Errorf("remove snapshots for %s; %w", id, err)
	}

	legacyPath := m.legacyRepoPath(id)
	if err := os.RemoveAll(legacyPath); err != nil {
		return fmt.Errorf("remove legacy clone %s; %w", legacyPath, err)
	}
	if repo.Path != "" && repo.Path != legacyPath && !pathWithin(repo.Path, snapshotRoot) && pathWithin(repo.Path, m.reposDir) {
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

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// WarmCodeSearch creates missing bw FTS indexes for repositories that were
// tracked before normal code search was introduced. Existing indexes are left
// untouched; regular refreshes keep them current afterwards.
//
// It is safe to run in the background: repos whose index is still missing are
// first marked pending, so a concurrent SearchCodeText for such a repo warms it
// on demand (ensureCodeIndex) and never returns partial results. Per-repo
// locking makes the background pass and an on-demand warm cooperate instead of
// double-indexing.
func (m *Manager) WarmCodeSearch(ctx context.Context) error {
	if m.codeText == nil {
		return nil
	}

	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	// Mark every repo with a missing index as pending up front, so a search
	// that races this pass knows to warm on demand rather than read an empty or
	// half-filled index.
	pending := make([]*registry.Repo, 0, len(repos))
	for _, repo := range repos {
		if repo.Path == "" || !fileExists(filepath.Join(repo.Path, ".git")) {
			continue
		}

		hasIndex, err := m.codeText.HasRepo(ctx, repo.ID)
		if err != nil {
			slog.Error("check code index", "repo", repo.ID, "error", err)
			continue
		}
		if hasIndex {
			continue
		}

		m.markCodeWarmPending(repo.ID)
		pending = append(pending, repo)
	}

	var errs []error
	for _, repo := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.ensureCodeIndex(ctx, repo.ID, repo.Path); err != nil {
			errs = append(errs, fmt.Errorf("warm code index for %s: %w", repo.ID, err))
		}
	}

	return errors.Join(errs...)
}

// markCodeWarmPending records that repo's normal code index still needs
// building, creating its per-repo warm lock if absent.
func (m *Manager) markCodeWarmPending(repoID string) {
	m.codeWarmMu.Lock()
	defer m.codeWarmMu.Unlock()
	if _, ok := m.codeWarm[repoID]; !ok {
		m.codeWarm[repoID] = &sync.Mutex{}
	}
}

// codeWarmLock returns the per-repo warm lock and whether repo is pending. When
// not pending, the index is already built (or was never scheduled) and callers
// need not warm it.
func (m *Manager) codeWarmLock(repoID string) (*sync.Mutex, bool) {
	m.codeWarmMu.Lock()
	defer m.codeWarmMu.Unlock()
	lk, ok := m.codeWarm[repoID]
	return lk, ok
}

// clearCodeWarmPending drops repo from the pending set once its index exists.
func (m *Manager) clearCodeWarmPending(repoID string) {
	m.codeWarmMu.Lock()
	defer m.codeWarmMu.Unlock()
	delete(m.codeWarm, repoID)
}

// ensureCodeIndex builds the normal (full-text) code index for repo if it is
// still pending, serialized per repo so the background warm pass and an
// on-demand warm triggered by a search never index the same repo twice. A
// no-op once the index exists.
func (m *Manager) ensureCodeIndex(ctx context.Context, repoID, clonePath string) error {
	if m.codeText == nil {
		return nil
	}

	lk, pending := m.codeWarmLock(repoID)
	if !pending {
		return nil
	}

	lk.Lock()
	defer lk.Unlock()

	// Re-check under the lock: another caller may have finished warming while we
	// waited, in which case the repo is no longer pending.
	if _, still := m.codeWarmLock(repoID); !still {
		return nil
	}

	if clonePath == "" || !fileExists(filepath.Join(clonePath, ".git")) {
		m.clearCodeWarmPending(repoID)
		return nil
	}

	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.codeRag == nil {
		return nil
	}

	if err := d.codeRag.IndexText(ctx, repoID, clonePath); err != nil {
		return err
	}

	m.clearCodeWarmPending(repoID)
	return nil
}

// TriggerRefresh queues a background refresh for a repo on the central work
// queue. Concurrent triggers for the same repo coalesce, and the queue's
// concurrency limit bounds how many repos refresh at once.
func (m *Manager) TriggerRefresh(id string) {
	m.submitRefresh(id)
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
// error joins the failed stages. When force is true the docs stage ignores its
// incremental caches and regenerates every summary and documentation.md even if
// nothing changed.
func (m *Manager) Generate(ctx context.Context, id string, targets []string, force bool) error {
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
			snapshot, err := m.prepareCurrentSnapshot(ctx, repo)
			if err != nil {
				serr = err
				st := repo.Stages.Get(registry.StageGraph)
				st.Status = registry.StageError
				st.Error = err.Error()
				st.Commit = repo.LastCommit
				st.FinishedAt = time.Now()
				_ = m.reg.Upsert(context.WithoutCancel(ctx), repo)
			} else {
				serr = m.buildGraphSnapshot(ctx, repo, snapshot, registry.StatusReady)
			}
			if serr == nil {
				if err := m.rebuildMerged(ctx); err != nil {
					slog.Error("rebuild merged graph", "error", err)
				}
			}

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

				_, err := d.gen.Generate(ctx, repo.ID, repo.Path, docsDir, force)

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

// generateTask builds the queue task for a selective generation, including its
// Spec (targets + force) so the work is persisted and rebuildable after a
// restart.
func (m *Manager) generateTask(id string, targets []string, force bool) queue.Task {
	return queue.Task{
		ID:    id,
		Kind:  taskKindGenerate,
		Title: fmt.Sprintf("Generate %s for %s", strings.Join(targets, ", "), id),
		Key:   fmt.Sprintf("%s:%s:%s:%t", taskKindGenerate, id, strings.Join(targets, ","), force),
		Spec: queue.Spec{
			Kind: taskKindGenerate,
			ID:   id,
			Params: map[string]string{
				"targets": strings.Join(targets, ","),
				"force":   strconv.FormatBool(force),
			},
		},
		Run: func(ctx context.Context) error {
			if err := m.Generate(ctx, id, targets, force); err != nil {
				slog.Error("generate", "repo", id, "targets", targets, "error", err)

				return err
			}

			return nil
		},
	}
}

// submitGenerate enqueues a selective generation on the central work queue and
// returns its handle. Identical requests (same repo, targets and force) coalesce
// onto one queued task.
func (m *Manager) submitGenerate(id string, targets []string, force bool) *queue.Handle {
	return m.queue.Submit(m.generateTask(id, targets, force))
}

// splitTargets parses a persisted comma-separated generate targets list,
// dropping blanks. The empty string yields no targets.
func splitTargets(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}

// TriggerGenerate queues a background selective generation for a repo. When
// force is true the docs stage ignores its incremental caches.
func (m *Manager) TriggerGenerate(id string, targets []string, force bool) {
	m.submitGenerate(id, targets, force)
}

// GenerateWait runs the selected generation stages for a repo in the background
// and waits until the run finishes or ctx is done, then returns the latest repo
// record. done reports whether the generation completed within the wait; when
// false the run continues detached and the record reflects the in-progress
// state. Stage failures are surfaced via the record's Status/LastError rather
// than a Go error; only unexpected lookup failures return an error.
func (m *Manager) GenerateWait(ctx context.Context, id string, targets []string, force bool) (*registry.Repo, bool, error) {
	// The generation runs on the manager lifecycle context: a client that stops
	// waiting (client-side tool timeout, ctrl+c, MCP cancellation) must not
	// kill the build mid-flight.
	finished := m.generateAsync(id, targets, force)

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

// generateAsync enqueues a selective generation and returns a channel closed
// when that run completes. Generate persists StatusError + LastError on failure,
// so callers read the terminal state back from the registry. During shutdown the
// channel is closed immediately.
func (m *Manager) generateAsync(id string, targets []string, force bool) <-chan struct{} {
	return m.submitGenerate(id, targets, force).Done()
}

// TriggerReindexAll rebuilds optional docs/code indexes for every ready repo
// and web source without fetching git or rebuilding graphify output. It is used
// after a live settings update because an ordinary refresh intentionally exits
// early when the repository commit has not changed.
//
// A lightweight coordinator task lists the work and enqueues one reindex task
// per repo/collection, so the global concurrency limit — not the repo count —
// governs how many run at once. A limit of 1 reindexes sequentially (the
// previous behavior, which avoided multiplying LLM/embedder load); a higher
// limit fans out within that bound.
func (m *Manager) TriggerReindexAll() {
	m.queue.Submit(m.reindexAllTask())
}

// reindexAllTask builds the reindex coordinator task with a Spec so a restart
// replays the whole reindex (which then re-enqueues per-repo/per-source work).
func (m *Manager) reindexAllTask() queue.Task {
	return queue.Task{
		ID:    "*",
		Kind:  taskKindReindex,
		Title: "Reindex all repositories and sources",
		Key:   taskKindReindex + ":all",
		Spec:  queue.Spec{Kind: taskKindReindex, ID: "*"},
		Run:   m.reindexAll,
	}
}

// reindexAll is the queue coordinator for TriggerReindexAll: it enqueues a
// reindex task for each ready repo and each web-source collection.
func (m *Manager) reindexAll(ctx context.Context) error {
	repos, err := m.reg.List(ctx)
	if err != nil {
		slog.Error("list repos for reindex", "error", err)
	} else {
		for _, listed := range repos {
			if listed.Status != registry.StatusReady {
				continue
			}

			m.scheduleReindex(listed.ID)
		}
	}

	// Web-source vectors live in the same docs index and follow the same
	// embedder settings, so they are rebuilt from the on-disk markdown too.
	m.enqueueWebReindex(ctx)

	return nil
}

// reindexTask builds a deduplicated reindex task for one repo (or, when id is a
// web-source scope key, one collection), carrying a Spec so a restart replays
// it. The rebuilt closure dispatches on the id's shape the same way.
func (m *Manager) reindexTask(id string) queue.Task {
	if name := websource.CollectionName(id); name != "" {
		scope := id

		return queue.Task{
			ID:    scope,
			Kind:  taskKindReindex,
			Title: "Reindex " + scope,
			Key:   taskKindReindex + ":" + scope,
			Spec:  queue.Spec{Kind: taskKindReindex, ID: scope},
			Run: func(ctx context.Context) error {
				l := m.lock(scope)
				l.Lock()
				defer l.Unlock()

				m.indexWebSource(ctx, name)

				return nil
			},
		}
	}

	return queue.Task{
		ID:    id,
		Kind:  taskKindReindex,
		Title: "Reindex " + id,
		Key:   taskKindReindex + ":" + id,
		Spec:  queue.Spec{Kind: taskKindReindex, ID: id},
		Run:   func(ctx context.Context) error { return m.reindexRepo(ctx, id) },
	}
}

// scheduleReindex enqueues a deduplicated background reindex of one repo's
// docs/code indexes. The queue key collapses repeats, so calling it while an
// identical reindex is already queued/running is a no-op.
func (m *Manager) scheduleReindex(id string) {
	m.queue.Submit(m.reindexTask(id))
}

// reindexRepo rebuilds a single ready repo's docs/code indexes from its
// existing clone, holding the per-repo lock.
func (m *Manager) reindexRepo(ctx context.Context, id string) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return err
	}

	if repo != nil && repo.Status == registry.StatusReady {
		// deferReindex=false: this already is the reindex path. Re-queueing on an
		// empty bundle here would busy-loop; recovery instead rides on the next
		// Configure/TriggerReindexAll once a healthy bundle is installed.
		m.buildDocsAndIndex(ctx, repo, false)
	}

	return nil
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

	hadGraph := fileExists(graphify.GraphPath(repo.Path))
	staleIgnore := hadGraph && m.gfy.GraphNeedsIgnoreRebuild(repo.Path)

	m.setActivity(repo.ID, "sync")
	snapshot, err := m.prepareSnapshot(ctx, repo, !hadGraph || staleIgnore)
	m.clearActivity(repo.ID, "sync")

	if err != nil {
		return err
	}

	repo.LastSyncAt = time.Now()

	if snapshot == nil {
		// Nothing new; keep current status.
		repo.Status = registry.StatusReady
		repo.LastError = ""

		slog.Info("repo already up to date, graph unchanged", "repo", repo.ID, "commit", shortSHA(repo.LastCommit))

		return m.reg.Upsert(ctx, repo)
	}

	if staleIgnore && snapshot.Commit == repo.LastCommit {
		slog.Info("graph contains now-excluded files; rebuilding to apply ignore rules", "repo", repo.ID)
	}

	repo.Status = registry.StatusBuilding
	repo.LastError = ""

	slog.Info("building graph snapshot", "repo", repo.ID, "path", snapshot.StagingPath, "commit", shortSHA(snapshot.Commit))

	buildStart := time.Now()

	if err := m.buildGraphSnapshot(ctx, repo, snapshot, registry.StatusReady); err != nil {
		return fmt.Errorf("graphify update; %w", err)
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
	// deferReindex=true: if the live bundle is momentarily empty (a Configure
	// swap coinciding with this post-graph phase), re-queue so a healthy bundle
	// finishes the indexes instead of leaving them silently missing.
	m.buildDocsAndIndex(ctx, repo, true)

	return nil
}

// buildDocsAndIndex regenerates markdown docs and refreshes the RAG indexes
// (docs + code) for a repo. All steps are optional and best-effort: a nil
// generator/service or an error is logged and swallowed so the graph build
// result stands.
func (m *Manager) buildDocsAndIndex(ctx context.Context, repo *registry.Repo, deferReindex bool) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.gen == nil && d.rag == nil && d.codeRag == nil {
		// A fully empty bundle is only ever the transient state produced by
		// Close (shutdown) or the brief window of a live Configure swap. If we
		// hit it here the graph build already flipped the repo to Ready, so a
		// silent return would leave docs/code indexes permanently missing with
		// no error trace until the next commit. Log it and, on the refresh path,
		// re-queue a reindex so a healthy bundle picks the work up. The reindex
		// path passes deferReindex=false to avoid busy-looping on itself.
		slog.Warn("docs/code bundle unavailable during index build",
			"repo", repo.ID, "requeued", deferReindex)
		if deferReindex {
			m.scheduleReindex(repo.ID)
		}

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
			// A normal refresh stays incremental; force is exposed only via the
			// explicit Generate path (refresh_repo force flag / API).
			man, gerr = d.gen.Generate(ctx, repo.ID, repo.Path, docsDir, false)

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
	oldSnapshotRoot := m.snapshotRoot(oldID)
	newSnapshotRoot := m.snapshotRoot(newID)
	switch {
	case repo.Path == "":
		repo.Path = newPath
	case pathWithin(repo.Path, oldSnapshotRoot):
		rel, err := filepath.Rel(oldSnapshotRoot, repo.Path)
		if err != nil {
			return fmt.Errorf("resolve snapshot path; %w", err)
		}
		if err := moveDir(oldSnapshotRoot, newSnapshotRoot); err != nil {
			return fmt.Errorf("move snapshots; %w", err)
		}
		repo.Path = filepath.Join(newSnapshotRoot, rel)
	case repo.Path != newPath && pathWithin(repo.Path, m.reposDir):
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
) error {
	st := repo.Stages.Get(registry.StageGraph)
	fail := func(err error, path string) error {
		if ctx.Err() != nil {
			err = ErrCancelled
		}
		st.Status = registry.StageError
		st.Error = err.Error()
		st.Commit = snapshot.Commit
		st.FinishedAt = time.Now()
		_ = m.reg.Upsert(context.WithoutCancel(ctx), repo)
		_ = os.RemoveAll(path)

		return err
	}

	st.Status = registry.StageRunning
	st.Error = ""
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return fail(err, snapshot.StagingPath)
	}

	m.setActivity(repo.ID, registry.StageGraph)
	defer m.clearActivity(repo.ID, registry.StageGraph)
	start := time.Now()
	err := m.gfy.Update(ctx, snapshot.StagingPath)
	if err != nil {
		return fail(err, snapshot.StagingPath)
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
	st.Status = registry.StageOK
	st.Error = ""
	st.Commit = snapshot.Commit
	st.FinishedAt = time.Now()

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
		m.engine.Invalidate(graphify.GraphPath(version.path))
		if err := os.RemoveAll(version.path); err != nil {
			slog.Warn("remove retired snapshot", "repo", id, "path", version.path, "error", err)
		}
	}

	legacyPath := m.legacyRepoPath(id)
	if filepath.Clean(legacyPath) == filepath.Clean(activePath) {
		return
	}
	if info, err := os.Stat(legacyPath); err == nil && time.Since(info.ModTime()) >= snapshotGracePeriod {
		m.engine.Invalidate(graphify.GraphPath(legacyPath))
		if err := os.RemoveAll(legacyPath); err != nil {
			slog.Warn("remove retired legacy clone", "repo", id, "path", legacyPath, "error", err)
		}
	}
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
		if err := os.Remove(m.mergedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty merged graph; %w", err)
		}
	case 1:
		// merge-graphs needs >=2 inputs; single repo = copy.
		if err := copyFile(graphs[0], m.mergedPath); err != nil {
			return fmt.Errorf("copy single graph; %w", err)
		}
	default:
		tmp := m.mergedPath + ".tmp"
		_ = os.Remove(tmp)
		if err := m.gfy.MergeGraphs(ctx, tmp, graphs...); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, m.mergedPath); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("publish merged graph; %w", err)
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
// the in-process native engine (no python serve process is spawned). When repoID
// is empty the candidate graphs are restricted to namespace (empty or "default"
// selects the default bucket; NamespaceAll searches every namespace).
func (m *Manager) CallGraphTool(ctx context.Context, repoID, namespace, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	if repoID == "" && !m.mergeEnabled {
		repoIDs, err := m.graphRepoIDs(ctx, namespace)
		if err != nil {
			return nil, err
		}

		switch len(repoIDs) {
		case 0:
			return nil, fmt.Errorf("no repository graph is ready in namespace %s; add a repository, wait for its build to finish, or retry with namespace \"*\"", displayNamespace(namespace))
		case 1:
			repoID = repoIDs[0]
		default:
			if inferred := inferRepoID(repoIDs, args); inferred != "" {
				repoID = inferred
			} else {
				return graphRepoSelectionResult(tool, namespace, repoIDs), nil
			}
		}
	}

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

func (m *Manager) graphRepoIDs(ctx context.Context, namespace string) ([]string, error) {
	repos, err := m.reg.List(ctx)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(repos))
	for _, repo := range repos {
		if !repoInNamespace(repo, namespace) {
			continue
		}
		if fileExists(graphify.GraphPath(repo.Path)) {
			ids = append(ids, repo.ID)
		}
	}
	sort.Strings(ids)

	return ids, nil
}

// repoInNamespace reports whether repo falls within the query namespace.
// NamespaceAll matches every repo; an empty or "default" query matches repos
// with no stored namespace; otherwise the stored namespace must match exactly.
func repoInNamespace(repo *registry.Repo, namespace string) bool {
	ns := strings.ToLower(strings.TrimSpace(namespace))
	if ns == registry.NamespaceAll {
		return true
	}
	if ns == "" || ns == registry.NamespaceDefault {
		return repo.Namespace == ""
	}

	return repo.Namespace == ns
}

// displayNamespace renders a namespace for messages, showing the default bucket
// as "default" rather than the empty stored form.
func displayNamespace(namespace string) string {
	ns := strings.ToLower(strings.TrimSpace(namespace))
	if ns == "" {
		return registry.NamespaceDefault
	}

	return ns
}

func inferRepoID(repoIDs []string, args map[string]any) string {
	var values []string
	for _, value := range args {
		if text, ok := value.(string); ok {
			values = append(values, text)
		}
	}
	haystack := normalizedMatchText(strings.Join(values, " "))

	match := ""
	for _, repoID := range repoIDs {
		name := repoID
		if slash := strings.LastIndex(name, "/"); slash >= 0 {
			name = name[slash+1:]
		}
		needle := normalizedMatchText(name)
		if len(needle) < 4 || !strings.Contains(haystack, needle) {
			continue
		}
		if match != "" {
			return ""
		}
		match = repoID
	}

	return match
}

func normalizedMatchText(value string) string {
	value = strings.ToLower(value)
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, value)
}

func graphRepoSelectionResult(tool, namespace string, repoIDs []string) *mcp.CallToolResult {
	const maxShown = 20
	shown := repoIDs
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}

	text := fmt.Sprintf(
		"Repository selection required: cross-repository merge is disabled and %d repository graphs are available in namespace %s. Retry %s with repo set to one of: %s.",
		len(repoIDs), displayNamespace(namespace), tool, strings.Join(shown, ", "))
	if len(repoIDs) > len(shown) {
		text += " Use list_repos with search to find additional repository ids."
	}
	text += " If the question does not identify a repository, ask the user which one they mean; for symbol or source lookup, search_code can search across repositories first."

	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// GraphEngine exposes the native graph engine for in-process consumers (docgen).
func (m *Manager) GraphEngine() *graphquery.Engine { return m.engine }

// repoCloneDir resolves a repo id to its on-disk clone directory, verifying the
// repo is tracked and has actually been cloned.
func (m *Manager) repoCloneDir(ctx context.Context, repoID string) (string, error) {
	dir, _, err := m.repoCloneDirAt(ctx, repoID, "")

	return dir, err
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

// ReadRepoFile returns the contents of a source file inside a tracked repo's
// clone. Access is sandboxed to the clone directory. offset/maxBytes paginate
// large files; maxBytes<=0 uses the repofs default cap.
func (m *Manager) ReadRepoFile(ctx context.Context, repoID, relPath string, offset int64, maxBytes int) (*repofs.FileContent, error) {
	return m.ReadRepoFileAt(ctx, repoID, relPath, "", offset, maxBytes)
}

// ReadRepoFileAt reads from a specific immutable snapshot when snapshot is set.
func (m *Manager) ReadRepoFileAt(ctx context.Context, repoID, relPath, snapshot string, offset int64, maxBytes int) (*repofs.FileContent, error) {
	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return nil, err
	}

	result, err := repofs.ReadFile(dir, relPath, offset, maxBytes)
	if err != nil {
		return nil, err
	}
	result.Snapshot = token

	return result, nil
}

// BlameCommit is the per-commit metadata referenced by blame hunks. It is held
// once in BlameFileResult.Commits and keyed by commit sha so the same commit is
// never repeated across hunks.
type BlameCommit struct {
	Author  string `json:"author"`
	Email   string `json:"email,omitempty"`
	Time    int64  `json:"time,omitempty"` // author time, unix seconds
	Summary string `json:"summary,omitempty"`
}

// BlameHunk is a run of consecutive lines attributed to the same commit.
type BlameHunk struct {
	Commit    string   `json:"commit"`     // sha; look up details in BlameFileResult.Commits
	LineStart int      `json:"line_start"` // 1-based, inclusive
	LineEnd   int      `json:"line_end"`   // 1-based, inclusive
	Lines     []string `json:"lines"`      // source lines for LineStart..LineEnd
}

// BlameFileResult carries structured git blame for a repo file. Consecutive
// lines from the same commit are grouped into hunks, and commit metadata is
// deduplicated into Commits (keyed by sha) so nothing is repeated.
type BlameFileResult struct {
	Repo     string                  `json:"repo"`
	Path     string                  `json:"path"`
	Start    int                     `json:"start,omitempty"`
	End      int                     `json:"end,omitempty"`
	Snapshot string                  `json:"snapshot,omitempty"`
	Commits  map[string]*BlameCommit `json:"commits"`
	Hunks    []BlameHunk             `json:"hunks"`
}

// BlameRepoFile runs `git blame` on a source file inside a tracked repo's clone.
// start/end limit the output to a line range (start<=0 blames the whole file,
// end<=0 blames start..EOF). When snapshot is set the blame is produced against
// that immutable snapshot; otherwise the active clone is used. Consecutive lines
// sharing a commit are collapsed into hunks and commit metadata is deduplicated.
func (m *Manager) BlameRepoFile(ctx context.Context, repoID, relPath, snapshot string, start, end int) (*BlameFileResult, error) {
	cleaned, err := repofs.CleanPath(relPath)
	if err != nil {
		return nil, err
	}

	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return nil, err
	}

	blameLines, err := m.git.Blame(ctx, dir, cleaned, start, end)
	if err != nil {
		return nil, err
	}

	res := &BlameFileResult{
		Repo:     repoID,
		Path:     cleaned,
		Start:    start,
		End:      end,
		Snapshot: token,
		Commits:  make(map[string]*BlameCommit),
	}

	for _, bl := range blameLines {
		if _, ok := res.Commits[bl.Commit]; !ok {
			res.Commits[bl.Commit] = &BlameCommit{
				Author:  bl.Author,
				Email:   bl.Email,
				Time:    bl.Time,
				Summary: bl.Summary,
			}
		}

		// Extend the current hunk when this line continues the same commit and
		// is contiguous; otherwise start a new hunk.
		if n := len(res.Hunks); n > 0 &&
			res.Hunks[n-1].Commit == bl.Commit &&
			res.Hunks[n-1].LineEnd+1 == bl.Line {
			res.Hunks[n-1].LineEnd = bl.Line
			res.Hunks[n-1].Lines = append(res.Hunks[n-1].Lines, bl.Content)

			continue
		}

		res.Hunks = append(res.Hunks, BlameHunk{
			Commit:    bl.Commit,
			LineStart: bl.Line,
			LineEnd:   bl.Line,
			Lines:     []string{bl.Content},
		})
	}

	return res, nil
}

// ListRepoFiles lists files under subdir ("" = repo root) in a tracked repo's
// clone. When recursive is true it walks the whole subtree.
func (m *Manager) ListRepoFiles(ctx context.Context, repoID, subdir string, recursive bool) ([]repofs.Entry, error) {
	entries, _, err := m.ListRepoFilesAt(ctx, repoID, subdir, "", recursive)

	return entries, err
}

// ListRepoFilesAt lists a specific immutable snapshot when snapshot is set.
func (m *Manager) ListRepoFilesAt(ctx context.Context, repoID, subdir, snapshot string, recursive bool) ([]repofs.Entry, string, error) {
	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return nil, "", err
	}

	entries, err := repofs.ListFiles(dir, subdir, recursive)

	return entries, token, err
}

// ListRepoFilesPage returns a bounded page of a stable repository listing.
func (m *Manager) ListRepoFilesPage(ctx context.Context, repoID, subdir string, recursive bool, page, perPage int) (repofs.EntryPage, error) {
	return m.ListRepoFilesPageAt(ctx, repoID, subdir, "", recursive, page, perPage)
}

// ListRepoFilesPageAt lists a specific immutable snapshot when snapshot is set.
func (m *Manager) ListRepoFilesPageAt(ctx context.Context, repoID, subdir, snapshot string, recursive bool, page, perPage int) (repofs.EntryPage, error) {
	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return repofs.EntryPage{}, err
	}

	result, err := repofs.ListFilesPage(dir, subdir, recursive, page, perPage)
	if err != nil {
		return repofs.EntryPage{}, err
	}
	result.Snapshot = token

	return result, nil
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
