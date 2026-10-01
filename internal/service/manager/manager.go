// Package manager orchestrates repository builds, indexes and query routing.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/worldline-go/types"
	"golang.org/x/sync/semaphore"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
)

// Manager coordinates registry, git, bag builds and the native graph
// query engine.
type Manager struct {
	reg          *registry.Registry
	git          *gitops.Git
	graphBuilder *graphbuilder.Builder
	engine       *graphquery.Engine
	creds        *credentials.Store
	externalMCPs *mcpclient.Store
	bigPictures  *bigpicture.Store
	codeText     *coderag.TextStore
	docsText     *rag.TextStore

	reposDir       string
	mergedPath     string
	mergeEnabled   bool
	docsRootDir    string
	docsVectorsDir string
	codeVectorsDir string
	sourcesRootDir string
	apisRootDir    string

	// Bundle resource hooks keep construction and shutdown in one ownership
	// boundary. Nil hooks use the package defaults in docs.go.
	openVectorStore        func(string) (vectorstore.Store, error)
	newLangfuseTracer      func(config.Langfuse) (*langfuse.Tracer, error)
	shutdownLangfuseTracer func(context.Context, *langfuse.Tracer) error

	// Web content sources (wiki pages, Confluence spaces). webFetchers maps a
	// collection type to its fetcher implementation; new source types register
	// here (see SetWebSources).
	webStore    *websource.Store
	webFetchers map[string]websource.Fetcher

	// API catalog (OpenAPI documents, gRPC servers). apiProviders maps a
	// service kind to its provider implementation; new kinds register here
	// (see SetAPICatalog).
	apiStore     *apicatalog.Store
	apiProviders map[string]apicatalog.Provider

	// queue is the central bounded work queue. Every background task (repo
	// refresh/generate, web-source sync, reindex) is submitted here so a single
	// configurable concurrency limit governs how many run at once, instead of
	// each trigger spawning its own unbounded goroutine.
	queue *queue.Queue

	// taskStore persists queued/running tasks so the backlog survives a
	// restart. Set via SetTaskStore before RestoreTasks; nil disables
	// persistence (tests, or when the store failed to open).
	taskStore TaskStore

	// bundleState owns leased access, replacement and resource shutdown.
	bundleState
	settings    *settings.Store
	settingsMu  sync.Mutex
	configureMu sync.Mutex

	baseCtx context.Context //nolint:containedctx // background lifecycle for async jobs

	locks       keyedLocks
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
	// docsTextWarmed records that the startup lexical backfill finished, and
	// docsTextKeys the keys already known to be indexed. Together they keep the
	// query path free of existence probes; see SearchDocs.
	docsTextWarmed atomic.Bool
	docsTextKeys   sync.Map

	progressMu sync.Mutex
	// progress is id -> phase -> counters. A repo runs code_index alongside
	// docs/docs_index (different backends, no data dependency), so an id can
	// have several phases in flight and a single slot per id would let them
	// overwrite each other.
	progress map[string]map[string]Progress

	// stageMu serializes stage-state mutation + persistence on the shared repo
	// record so stages may run concurrently for the same repo.
	stageMu sync.Mutex

	// jobs tracks the cancel function of the currently running refresh/generate
	// job per repo so users can abort long builds manually.
	jobMu sync.Mutex
	jobs  map[string]*job

	// codeWarm tracks background warming of the normal (full-text) code search
	// index. pending holds repo ids whose index has not been built yet; a
	// per-repo mutex (inflight) serializes on-demand warming so a search that
	// races the background pass never returns partial results. See
	// WarmCodeSearch and ensureCodeIndex.
	codeWarmMu sync.Mutex
	codeWarm   map[string]*semaphore.Weighted
}

// DocsDeps carries the immutable wiring for the docs/RAG subsystem.
type DocsDeps struct {
	// TextStore is the persistent BM25 index for generated docs and web sources.
	TextStore *rag.TextStore
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
	// APIsRootDir stores the API catalog's per-operation markdown projections
	// by service name. They are indexed beside repo docs and web sources, so
	// an endpoint is findable by the same search that finds prose about it.
	APIsRootDir string
}

// New creates a Manager. baseCtx bounds background refresh jobs. docs carries the
// docs/RAG wiring; the clients themselves are built later via Configure.
func New(
	baseCtx context.Context,
	reg *registry.Registry,
	git *gitops.Git,
	graphBuilder *graphbuilder.Builder,
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
		graphBuilder:   graphBuilder,
		engine:         engine,
		creds:          creds,
		codeText:       codeText,
		docsText:       docs.TextStore,
		reposDir:       reposDir,
		mergedPath:     mergedPath,
		mergeEnabled:   mergeEnabled,
		docsRootDir:    docs.DocsRootDir,
		docsVectorsDir: docs.DocsVectorsDir,
		codeVectorsDir: docs.CodeVectorsDir,
		sourcesRootDir: docs.SourcesRootDir,
		apisRootDir:    docs.APIsRootDir,
		bundleState: bundleState{docs: &docsBundle{
			codeRag: coderag.New(config.CodeRAG{}, nil, nil, engine, codeText),
		}},
		baseCtx:  baseCtx,
		activity: map[string]map[string]struct{}{},
		progress: map[string]map[string]Progress{},
		jobs:     map[string]*job{},
		codeWarm: map[string]*semaphore.Weighted{},
	}
	// The queue's limit is updated from persisted settings at startup and on
	// every settings change (see SetTaskConcurrency); it starts at the default.
	m.queue = queue.New(baseCtx, queue.DefaultConcurrency)
	return m
}

// SetCredential validates and stores transport-supplied credential material.
// Keeping this operation on Manager prevents transports from bypassing policy
// added around credential writes later.
func (m *Manager) SetCredential(ctx context.Context, cred *credentials.Credential) error {
	return m.creds.Set(ctx, cred)
}

// ListCredentials returns the stored credential metadata. Credential.Secret is
// excluded from JSON serialization by the credentials package.
func (m *Manager) ListCredentials(ctx context.Context) ([]*credentials.Credential, error) {
	return m.creds.List(ctx)
}

// DeleteCredential removes one credential through the manager boundary.
func (m *Manager) DeleteCredential(ctx context.Context, pattern string) error {
	return m.creds.Delete(ctx, pattern)
}

// Wait blocks until in-flight background jobs finish.
func (m *Manager) Wait() { m.wg.Wait() }

// Close waits for background work, flushes the active tracer and releases active
// vector stores. It is safe to call once server shutdown has stopped accepting
// new manager operations.
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

	return m.bundleState.replace(&docsBundle{})
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

	errs = append(errs, m.reconcileInterruptedSyncs(ctx))

	return errors.Join(errs...)
}

// reconcileInterruptedSyncs clears the "fetching" status left by a sync that
// died with the process. The status is a lock as much as a label: the interval
// poll skips a collection that claims to be fetching, so a kill (an OOM during
// a large sweep is the likely one) would otherwise park the source forever,
// with the UI polling it every two seconds because it looks live.
func (m *Manager) reconcileInterruptedSyncs(ctx context.Context) error {
	if m.webStore == nil {
		return nil
	}

	cols, err := m.webStore.ListCollections(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, col := range cols {
		if col.Status != websource.StatusFetching {
			continue
		}

		col.Status = websource.StatusError
		col.LastError = "interrupted by service restart"
		// Do not stamp LastRefreshAt: nothing was completed, and the next poll
		// should pick this collection straight back up.
		if err := m.webStore.UpsertCollection(ctx, col); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// lockKey acquires the per-key mutex and returns its release function, so the
// only way to obtain an unlock is to have taken the lock first:
//
//	defer m.lockKey(repo.ID)()
//
// This shape exists because the per-key mutexes are held across whole pipeline
// stages, so obtaining one and unlocking it without having locked it is easy to
// write and catastrophic to run: Go answers an unlock of an unlocked mutex with
// a fatal error that no recover can contain. Handing back a release that is
// bound to a completed acquisition removes the opportunity.
//
// Keys are repository ids and web-source scopes; they share one namespace, so a
// key must be distinct enough not to collide with another subsystem's (see
// lockRecord, which suffixes its own).
func (m *Manager) lockKey(key string) func() {
	return m.locks.acquire(key)
}

// tryLockKey is lockKey for callers that must be able to walk away when the
// lock is already held rather than queue behind it.
func (m *Manager) tryLockKey(key string) (func(), bool) {
	return m.locks.tryAcquire(key)
}

// RepoSpec describes a repository to track. Only URL is required.
//
// Namespace and Overrides apply to a newly registered repo only: adding a repo
// that is already tracked stays the idempotent "just refresh it" operation it
// has always been, rather than silently rewriting the configuration someone
// tuned earlier. Use SetRepoNamespace / SetRepoOverrides to change those.
type RepoSpec struct {
	URL       string
	Branch    string
	Namespace string
	Overrides registry.Overrides
}

// AddRepo registers a repository and starts a background clone+build.
// If the repo already exists, it just triggers a refresh — which is what makes
// this the usual "add or re-pull" entry point, so it takes the same per-run
// skip list as TriggerRefresh. skip applies to the queued build only; use the
// spec's Overrides.SkipStages to turn a stage off for good. A persistence
// failure is returned; a newly registered repo is retained in StatusError so it
// cannot remain indefinitely pending with no queued work.
func (m *Manager) AddRepo(ctx context.Context, spec RepoSpec, skip ...string) (*registry.Repo, error) {
	id, repo, existed, err := m.registerRepo(ctx, spec)
	if err != nil {
		return nil, err
	}

	if err := m.TriggerRefresh(id, skip...); err != nil {
		return repo, m.recordRepoEnqueueFailure(ctx, repo, existed, err)
	}

	return repo, nil
}

// AddRepoWait registers a repository, starts the clone+build in the background
// and waits for it to finish for as long as ctx allows. The build itself runs
// detached from ctx, so a caller cancel or timeout never aborts it: when the
// wait ends early the current in-progress record is returned with done=false
// and the build keeps running (poll repo_status for the final state).
//
// A build failure is reported through the returned record's Status ("error") and
// LastError, not as a Go error, so callers always get the final state back. An
// enqueue failure is returned immediately.
// skip drops stages from this build only; see AddRepo.
func (m *Manager) AddRepoWait(ctx context.Context, spec RepoSpec, skip ...string) (*registry.Repo, bool, error) {
	id, repo, existed, err := m.registerRepo(ctx, spec)
	if err != nil {
		return nil, false, err
	}

	registered := repo
	repo, done, err := m.RefreshWait(ctx, id, skip...)
	if err != nil {
		return registered, done, m.recordRepoEnqueueFailure(ctx, registered, existed, err)
	}

	return repo, done, nil
}

func (m *Manager) recordRepoEnqueueFailure(
	ctx context.Context, repo *registry.Repo, existed bool, enqueueErr error,
) error {
	if existed || repo == nil {
		return enqueueErr
	}

	repo.Status = registry.StatusError
	repo.LastError = "enqueue refresh: " + enqueueErr.Error()
	if err := m.reg.Upsert(context.WithoutCancel(ctx), repo); err != nil {
		return errors.Join(enqueueErr, fmt.Errorf("save repo enqueue failure; %w", err))
	}

	return enqueueErr
}

// registerRepo parses the url, upserts a pending record if the repo is new, and
// reports whether it already existed. It performs no clone/build itself.
//
// A call naming overrides for a repository that is already tracked applies
// them instead of dropping them. Dropping was the older behaviour and it
// returned success: an agent asked to also index a repo's deployment YAML
// called add_repo with include_extra, got the repo record back, and reported
// the repository reconfigured while nothing had changed. Overrides the caller
// did not name are left alone - an add_repo that omits them must not wipe what
// set_repo_overrides put there - and an identical write is a no-op inside
// SetOverrides, so a client that always sends the same block costs nothing.
// The refresh AddRepo queues next is what rebuilds against the new rules.
func (m *Manager) registerRepo(ctx context.Context, spec RepoSpec) (id string, repo *registry.Repo, existed bool, err error) {
	id, err = gitops.ParseRepoID(spec.URL)
	if err != nil {
		return "", nil, false, err
	}

	if strings.TrimSpace(spec.Namespace) == registry.NamespaceAll {
		return "", nil, false, fmt.Errorf("namespace %q is reserved", registry.NamespaceAll)
	}

	existing, gerr := m.reg.Get(ctx, id)
	if gerr != nil {
		return "", nil, false, gerr
	}
	if existing != nil {
		if !spec.Overrides.Changed(registry.Overrides{}) {
			return id, existing, true, nil
		}

		// Under the per-repo lock for the same reason SetRepoOverrides takes
		// it: a build reads the overrides it started with, and a half-applied
		// change is a build against rules nobody asked for.
		updated, _, serr := func() (*registry.Repo, registry.Overrides, error) {
			defer m.lockKey(id)()

			return m.reg.SetOverrides(ctx, id, spec.Overrides)
		}()
		if serr != nil {
			return "", nil, false, serr
		}

		return id, updated, true, nil
	}

	repo = &registry.Repo{
		ID:        id,
		URL:       spec.URL,
		Branch:    spec.Branch,
		Path:      filepath.Join(m.reposDir, filepath.FromSlash(id)),
		Status:    registry.StatusPending,
		Namespace: registry.NormalizeNamespace(spec.Namespace),
		Overrides: spec.Overrides.Normalize(),
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

	defer m.lockKey(repo.ID)()

	return m.reg.SetNamespace(ctx, repo.ID, namespace)
}

// SetRepoOverrides replaces one repository's indexing/documentation overrides.
//
// A change invalidates what was already built from the old rules — the code
// index holds files the new selection excludes, and documentation.md was
// written against the old prompt — so it queues a rebuild of exactly the
// artifacts affected: a graph-exclude change needs a full refresh, anything
// else only the docs/index pass. An identical write rebuilds nothing, so a UI
// that saves the whole form on every edit is free.
func (m *Manager) SetRepoOverrides(ctx context.Context, ref string, over registry.Overrides) (*registry.Repo, error) {
	repo, err := m.reg.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("repo %s not found", ref)
	}

	// Serialize against the pipeline: a build reads the overrides it was
	// started with, so writing them under the same per-repo lock keeps a
	// refresh from picking up half of a change. The rebuild triggers below
	// take this lock themselves and must stay outside it.
	updated, prev, err := func() (*registry.Repo, registry.Overrides, error) {
		defer m.lockKey(repo.ID)()

		return m.reg.SetOverrides(ctx, repo.ID, over)
	}()

	if err != nil {
		return nil, err
	}

	switch {
	case prev.GraphChanged(updated.Overrides):
		// The graph itself is now wrong, and every artifact derived from it
		// with it. A full refresh is the only path that rebuilds the graph;
		// it detects the stale ignore block and forces the rebuild.
		if err := m.TriggerRefresh(updated.ID); err != nil {
			return updated, fmt.Errorf("repo overrides saved but refresh enqueue failed; %w", err)
		}
	case prev.Changed(updated.Overrides):
		// Selection or prompt only: the graph still stands, so rebuild just the
		// docs and indexes derived from it.
		if err := m.scheduleReindex(updated.ID); err != nil {
			return updated, fmt.Errorf("repo overrides saved but reindex enqueue failed; %w", err)
		}
	}

	return updated, nil
}

// repoFilters resolves the file-selection overrides of one repository. A nil
// repo (or one that overrides nothing) inherits the install-wide filters.
func repoFilters(repo *registry.Repo) config.Filters {
	if repo == nil {
		return config.Filters{}
	}

	return config.Filters{
		Include:      repo.Overrides.Include,
		IncludeExtra: repo.Overrides.IncludeExtra,
		Exclude:      repo.Overrides.Exclude,
	}
}

// repoDocsOverride resolves the documentation overrides of one repository.
func repoDocsOverride(repo *registry.Repo) config.DocsOverride {
	if repo == nil {
		return config.DocsOverride{}
	}

	return config.DocsOverride{
		Filters:     repoFilters(repo),
		Prompt:      repo.Overrides.DocsPrompt,
		PromptExtra: repo.Overrides.DocsPromptExtra,
		Limits: config.DocsLimits{
			MaxSourceBytes:    repo.Overrides.DocsMaxSourceBytes,
			MaxGroupBytes:     repo.Overrides.DocsMaxGroupBytes,
			MaxSynthesisBytes: repo.Overrides.DocsMaxSynthesisBytes,
		},
	}
}

// UpsertNamespace creates or updates the description metadata for a namespace.
// An unset description keeps the stored one; see registry.UpsertNamespace.
func (m *Manager) UpsertNamespace(ctx context.Context, name string, description types.Null[string]) (*registry.NamespaceRecord, error) {
	return m.reg.UpsertNamespace(ctx, name, description)
}

// DeleteNamespace removes a namespace's description record. Repos keep their tag.
func (m *Manager) DeleteNamespace(ctx context.Context, name string) error {
	return m.reg.DeleteNamespace(ctx, name)
}

// RemoveRepo deletes the record, local clone, generated docs and derived indexes.
func (m *Manager) RemoveRepo(ctx context.Context, id string) error {
	defer m.lockKey(id)()

	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return err
	}

	if repo == nil {
		return fmt.Errorf("repo %s not found", id)
	}

	m.engine.Invalidate(graphbuilder.GraphPath(repo.Path))

	// Best-effort: drop the repo's vectors from the RAG indexes.
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.rag != nil {
		if err := d.rag.DeleteRepo(ctx, id); err != nil {
			slog.Error("delete repo from rag index", "repo", id, "error", err)
		}
	}
	if m.docsText != nil {
		if err := m.docsText.DeleteRepo(ctx, id); err != nil {
			slog.Error("delete repo from docs text index", "repo", id, "error", err)
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

// Repo returns a tracked repository by its canonical id.
func (m *Manager) Repo(ctx context.Context, id string) (*registry.Repo, error) {
	return m.reg.Get(ctx, id)
}

// ResolveRepo resolves a canonical id or an unambiguous legacy suffix.
func (m *Manager) ResolveRepo(ctx context.Context, ref string) (*registry.Repo, error) {
	return m.reg.Resolve(ctx, ref)
}

// ListRepos returns one filtered page of tracked repositories.
func (m *Manager) ListRepos(ctx context.Context, opts registry.ListOptions) ([]*registry.Repo, int, error) {
	return m.reg.ListPaged(ctx, opts)
}

// RepoOwners returns repository owner groups for transport discovery views.
func (m *Manager) RepoOwners(ctx context.Context) ([]registry.OwnerGroup, error) {
	return m.reg.Owners(ctx)
}

// RepoNamespaces returns repository namespace groups and descriptions.
func (m *Manager) RepoNamespaces(ctx context.Context) ([]registry.NamespaceGroup, error) {
	return m.reg.Namespaces(ctx)
}
