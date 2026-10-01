package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
)

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

// skipsStage reports whether a repository opted out of a pipeline stage. A nil
// repo skips nothing, so callers that could not load the record keep the
// previous, always-run behaviour rather than silently doing less work.
func skipsStage(repo *registry.Repo, stage string) bool {
	return repo != nil && repo.Overrides.SkipsStage(stage)
}

// stageSkipCascade maps a stage to the stages that must be skipped along with
// it because they consume its output directly: indexing documentation that was
// not regenerated only re-embeds the previous run's markdown at full cost.
//
// The graph is deliberately absent. Docs and code_index degrade gracefully
// without it — one summary call per file, line-window chunking — so skipping
// the graph still lets them run, the same contract resolveStageDeps applies to
// the persisted skip_stages override.
var stageSkipCascade = map[string][]string{
	registry.StageDocs: {registry.StageDocsIndex},
}

// runSkips is the set of stages a single refresh run opts out of. It exists
// because skip_stages is persistent: turning docs off for one quick refresh
// otherwise means editing the repo overrides and remembering to put them back.
// A run's skips are additive to the record's — a run may do less than the
// overrides allow, never more.
type runSkips map[string]struct{}

// newRunSkips normalizes the requested stage names and expands them over
// stageSkipCascade. Unknown names are dropped by registry.NormalizeStages;
// callers that must reject typos validate before calling (see the REST and MCP
// refresh handlers). A nil result means "skip nothing extra".
func newRunSkips(stages []string) runSkips {
	names := registry.NormalizeStages(stages)
	if len(names) == 0 {
		return nil
	}

	skips := make(runSkips, len(names))

	var add func(string)
	add = func(name string) {
		if _, ok := skips[name]; ok {
			return
		}
		skips[name] = struct{}{}
		for _, dependent := range stageSkipCascade[name] {
			add(dependent)
		}
	}

	for _, name := range names {
		add(name)
	}

	return skips
}

// skips reports whether stage must not run, either because this run asked to
// skip it or because the repository opted out of it permanently.
func (s runSkips) skips(repo *registry.Repo, stage string) bool {
	if _, ok := s[stage]; ok {
		return true
	}

	return skipsStage(repo, stage)
}

// list returns the run's skipped stages in a stable order, for the queue dedup
// key, the persisted spec and log lines.
func (s runSkips) list() []string {
	if len(s) == 0 {
		return nil
	}

	out := make([]string, 0, len(s))
	for name := range s {
		out = append(out, name)
	}
	slices.Sort(out)

	return out
}

// invalidateStage clears a stage's recorded success so a later run cannot
// mistake it for up to date. A per-run skip is the only way a stage's input can
// move forward while the stage itself stays untouched, and buildDocsAndIndex's
// docs-unchanged shortcut trusts StageOK — without this the docs index would
// stay stale for every future refresh, not just the one that skipped it.
func (m *Manager) invalidateStage(ctx context.Context, repo *registry.Repo, name string) {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()

	st := repo.Stages.Get(name)
	if st == nil || st.Status != registry.StageOK {
		return
	}

	*st = registry.StageState{}
	if err := m.reg.Upsert(context.WithoutCancel(ctx), repo); err != nil {
		slog.Error("invalidate stage state", "repo", repo.ID, "stage", name, "error", err)
	}
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
		return fileExists(graphbuilder.GraphPath(repo.Path))
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

			// A stage the repository opted out of is never pulled back in as
			// somebody else's prerequisite. Both dependents of the graph
			// degrade gracefully without it — docs fall back to one summary
			// call per file, code chunking to line windows — so the dependent
			// still runs, just without graph anchoring.
			if skipsStage(repo, dep) {
				slog.Info("stage dependency skipped by repo override",
					"repo", id, "stage", name, "dependency", dep)

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

	defer m.lockKey(id)()

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

	// A stage this repository opted out of is not silently turned into a no-op
	// that reports success: an explicit request for it is an error naming the
	// override, so the caller can either drop the target or clear the skip.
	var blocked []string
	for _, name := range generateOrder {
		if want[name] && skipsStage(repo, name) {
			blocked = append(blocked, name)
		}
	}

	if len(blocked) > 0 {
		return fmt.Errorf("stage(s) %s are disabled for %s by its skip_stages override; clear it with set_repo_overrides to run them",
			strings.Join(blocked, ", "), id)
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
				// Logged rather than returned: serr is the failure the caller
				// needs to see. But losing this write silently leaves the stage
				// looking like it never failed, so it must not vanish.
				if err := m.reg.Upsert(context.WithoutCancel(ctx), repo); err != nil {
					slog.Error("persist graph stage failure", "repo", repo.ID, "error", err)
				}
			} else {
				// Generate is already an explicit stage selection, so it adds
				// no run-level skips on top of the repo's overrides.
				serr = m.buildGraphSnapshot(ctx, repo, snapshot, registry.StatusReady, nil)
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
			serr = m.runCodeIndexStage(ctx, repo, d, codeIndexOptions{})
		case registry.StageDocs:
			_, serr = m.runDocsStage(ctx, repo, d, docsDir, docsDirErr, docsStageOptions{force: force})

			// A failed docs run leaves no fresh markdown to index.
			if serr != nil && want[registry.StageDocsIndex] {
				want[registry.StageDocsIndex] = false
				errs = append(errs, fmt.Errorf("docs_index skipped: docs generation failed"))
			}
		case registry.StageDocsIndex:
			serr = m.runDocsIndexStage(ctx, repo, d, docsDir, docsDirErr)
		}

		if serr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, serr))
		}
	}

	return errors.Join(errs...)
}

// reindexAll is the queue coordinator for TriggerReindexAll: it enqueues a
// reindex task for each ready repo and each web-source collection.
func (m *Manager) reindexAll(ctx context.Context) error {
	var errs []error
	repos, err := m.reg.List(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list repos for reindex; %w", err))
	} else {
		for _, listed := range repos {
			if listed.Status != registry.StatusReady {
				continue
			}

			if err := m.scheduleReindex(listed.ID); err != nil {
				errs = append(errs, fmt.Errorf("enqueue reindex for repo %s; %w", listed.ID, err))
			}
		}
	}

	// Web-source and API-catalog vectors live in the same docs index and follow
	// the same embedder settings, so they are rebuilt from the on-disk markdown
	// too.
	if err := m.enqueueWebReindex(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := m.enqueueAPIReindex(ctx); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// reindexRepo rebuilds a single ready repo's docs/code indexes from its
// existing clone, holding the per-repo lock.
func (m *Manager) reindexRepo(ctx context.Context, id string) error {
	defer m.lockKey(id)()

	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return err
	}

	if repo != nil && repo.Status == registry.StatusReady {
		// deferReindex=false: this already is the reindex path. Re-queueing on an
		// empty bundle here would busy-loop; recovery instead rides on the next
		// Configure/TriggerReindexAll once a healthy bundle is installed.
		m.buildDocsAndIndex(ctx, repo, nil, false, true)
	}

	return nil
}

// Refresh synchronously clones/pulls the repo and rebuilds its graph if needed.
// skip names stages this run must not execute (see runSkips), on top of the
// repo's persisted skip_stages override.
func (m *Manager) Refresh(ctx context.Context, id string, skip ...string) error {
	defer m.lockKey(id)()

	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return err
	}

	if repo == nil {
		return fmt.Errorf("repo %s not found", id)
	}

	jobCtx, finish := m.registerJob(ctx, id)
	defer finish()

	if err := m.refresh(jobCtx, repo, newRunSkips(skip)); err != nil {
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

func (m *Manager) refresh(ctx context.Context, repo *registry.Repo, skips runSkips) error {
	slog.Info("working on repo", "repo", repo.ID, "commit", shortSHA(repo.LastCommit),
		"skip", skips.list())

	defer m.clearRepoActivity(repo.ID)

	// A run that skips the graph stage has no graph to be missing or stale, so
	// none of these may force a snapshot rebuild — otherwise every refresh
	// would re-clone forever chasing a graph it will never build. A per-run
	// skip needs no special care afterwards: the promoted clone carries no
	// graph.json, so the next unskipped refresh sees hadGraph=false and forces
	// the rebuild itself.
	graphSkipped := skips.skips(repo, registry.StageGraph)
	hadGraph := graphSkipped || fileExists(graphbuilder.GraphPath(repo.Path))
	staleIgnore := !graphSkipped && hadGraph && m.graphBuilder.GraphNeedsIgnoreRebuild(repo.Path, repo.Overrides.GraphExclude)
	staleVersion := !graphSkipped && hadGraph && !m.graphBuilder.GraphBuiltWithCurrentVersion(repo.Path)

	m.setActivity(repo.ID, stepSync)
	snapshot, err := m.prepareSnapshot(ctx, repo, !hadGraph || staleIgnore || staleVersion)
	m.clearActivity(repo.ID, stepSync)

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
	if staleVersion && snapshot.Commit == repo.LastCommit {
		slog.Info("graph was built by a different bag engine version; rebuilding",
			"repo", repo.ID, "graph_engine_version", m.graphBuilder.Version())
	}

	repo.Status = registry.StatusBuilding
	repo.LastError = ""

	slog.Info("building graph snapshot", "repo", repo.ID, "path", snapshot.StagingPath, "commit", shortSHA(snapshot.Commit))

	buildStart := time.Now()

	if err := m.buildGraphSnapshot(ctx, repo, snapshot, registry.StatusReady, skips); err != nil {
		return fmt.Errorf("build graph; %w", err)
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
	// deferReindex=true: if the live bundle is unavailable, re-queue so a healthy
	// bundle finishes the indexes instead of leaving them silently missing.
	m.buildDocsAndIndex(ctx, repo, skips, true, false)

	return nil
}

// buildDocsAndIndex regenerates markdown docs and refreshes the RAG indexes
// (docs + code) for a repo. All steps are optional and best-effort: a nil
// generator/service or an error is logged and swallowed so the graph build
// result stands. forceIndexes bypasses incremental/presence shortcuts after a
// settings change, ensuring a newly enabled embedder or changed model/chunking
// configuration replaces every derived vector. skips drops stages for this run
// only; a nil skips honours just the repository's persisted skip_stages.
func (m *Manager) buildDocsAndIndex(
	ctx context.Context,
	repo *registry.Repo,
	skips runSkips,
	deferReindex, forceIndexes bool,
) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.gen == nil && d.rag == nil && d.codeRag == nil && m.docsText == nil {
		// An unavailable bundle may have been detached by Close. If we
		// hit it here the graph build already flipped the repo to Ready, so a
		// silent return would leave docs/code indexes permanently missing with
		// no error trace until the next commit. Log it and, on the refresh path,
		// re-queue a reindex so a healthy bundle picks the work up. The reindex
		// path passes deferReindex=false to avoid busy-looping on itself.
		slog.Warn("docs/code bundle unavailable during index build",
			"repo", repo.ID, "requeued", deferReindex)
		if deferReindex {
			if err := m.scheduleReindex(repo.ID); err != nil {
				slog.Error("enqueue deferred repo reindex", "repo", repo.ID, "error", err)
			}
		}

		return
	}

	// code_index and docs have no data dependency (both need only the clone +
	// graph) and hit different backends (embedder vs LLM), so they run in
	// parallel; runStage serializes their stage-state writes on stageMu.
	var wg sync.WaitGroup
	if d.codeRag != nil && !skips.skips(repo, registry.StageCodeIndex) {
		// The delta must be captured before runStage mutates the stage state.
		var changed []string
		incremental := false
		if !forceIndexes {
			changed, incremental = m.codeIndexDelta(ctx, repo)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			//nolint:errcheck // recorded on the stage state; never fails the build
			_ = m.runCodeIndexStage(ctx, repo, d, codeIndexOptions{
				changed: changed, incremental: incremental,
				reportProgress: true, refreshStats: true,
			})
		}()
	}
	defer wg.Wait()

	if d.gen == nil && d.rag == nil && m.docsText == nil {
		return
	}

	if skips.skips(repo, registry.StageDocs) && skips.skips(repo, registry.StageDocsIndex) {
		return
	}

	docsDir, err := m.docsDirForRepo(repo)
	if err != nil {
		slog.Error("resolve generated docs directory", "repo", repo.ID, "error", err)

		return
	}

	docsChanged := true
	docsRan := false
	if d.gen != nil && !skips.skips(repo, registry.StageDocs) {
		// Refresh stays incremental; force is exposed only by Generate.
		man, err := m.runDocsStage(ctx, repo, d, docsDir, nil, docsStageOptions{reportProgress: true})
		if err != nil {
			return // no fresh docs -> skip indexing
		}

		docsRan = true
		if man != nil {
			docsChanged = man.ChangedDocs
		}
	}

	if d.rag == nil && m.docsText == nil {
		return
	}

	if skips.skips(repo, registry.StageDocsIndex) {
		// Docs moved forward but their index did not, so the shortcut below
		// must not read the previous success as "still current" on the next
		// refresh — that would strand the index on this run's stale markdown.
		if docsRan && docsChanged {
			m.invalidateStage(ctx, repo, registry.StageDocsIndex)
		}

		return
	}

	// Unchanged documentation with a previously successful stage needs no
	// rebuild only when every configured search index still holds this repo.
	if st := repo.Stages.Get(registry.StageDocsIndex); !forceIndexes && !docsChanged && st.Status == registry.StageOK {
		indexesPresent := true
		if d.rag != nil {
			has, err := d.rag.HasRepo(ctx, repo.ID)
			if err != nil {
				slog.Warn("docs vector index presence check failed; reindexing", "repo", repo.ID, "error", err)
				indexesPresent = false
			} else if !has {
				indexesPresent = false
			}
		}
		if m.docsText != nil {
			has, err := m.docsText.HasRepo(ctx, repo.ID)
			if err != nil {
				slog.Warn("docs text index presence check failed; reindexing", "repo", repo.ID, "error", err)
				indexesPresent = false
			} else if !has {
				indexesPresent = false
			}
		}
		if indexesPresent {
			slog.Info("documentation unchanged, skipping docs index", "repo", repo.ID)

			return
		}
		slog.Info("docs index missing for repo despite ok stage; reindexing", "repo", repo.ID)
	}

	//nolint:errcheck // recorded on the stage state; never fails the build
	_ = m.runDocsIndexStage(ctx, repo, d, docsDir, nil)
}

// indexDocs rebuilds every configured documentation search index. Lexical
// search remains available when RAG is disabled or has no embedder configured.
func (m *Manager) indexDocs(ctx context.Context, d *docsBundle, repo, docsDir string) error {
	var errs []error
	if m.docsText != nil {
		if err := m.docsText.IndexWithOptions(ctx, repo, docsDir, &rag.IndexOptions{
			KeepMarkdownTargets: d.ragCfg.KeepMarkdownTargets,
		}); err != nil {
			errs = append(errs, fmt.Errorf("index docs text; %w", err))
		}
		// Query tuning is derived from the corpus, so it follows the corpus.
		// A failure here only costs lexical query speed, never correctness.
		if err := m.docsText.RefreshStats(ctx); err != nil {
			slog.Warn("refresh docs search stats", "repo", repo, "error", err)
		}
	}
	if d.rag != nil {
		// Only the vector arm is worth reporting: the lexical index is local
		// work that finishes in a fraction of the embedding time.
		defer m.clearProgress(repo, registry.StageDocsIndex)

		if err := d.rag.IndexProgress(ctx, repo, docsDir, m.progressReporter(repo, registry.StageDocsIndex)); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
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
