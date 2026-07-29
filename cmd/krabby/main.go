// Command krabby serves multi-repo graphify knowledge graphs over MCP.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rakunlabs/into"
	"github.com/rakunlabs/logi"
	"github.com/rakunlabs/tell"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/server"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/mcptools"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/scheduler"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/taskstore"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/confluence"
	"github.com/rytsh/krabby/internal/service/websource/jira"
	"github.com/rytsh/krabby/internal/service/websource/pages"
	"github.com/rytsh/krabby/internal/storage"
)

// Injected at build time via -ldflags.
var (
	version = "v0.0.0"
	commit  = "-"
	date    = "-"
)

func main() {
	config.Version = version
	config.Commit = commit
	config.Date = date

	into.Init(run,
		into.WithLogger(logi.InitializeLog(logi.WithCaller(false))),
		into.WithMsgf("%s version:[%s] commit:[%s] date:[%s]",
			config.ServiceName, version, commit, date),
	)
}

func run(ctx context.Context) error {
	cfg, err := config.Load(ctx)
	if err != nil {
		return err
	}

	// Resolve the memory budget before anything allocates. It installs Go's
	// soft heap limit and sizes the embedded databases' caches, the parsed
	// graph cache and the indexing batches; without it the defaults of three
	// Badger stores plus an unbounded GC target exceed a typical container
	// limit on their own.
	budget := memlimit.NewWithOverrides(cfg.Memory.LimitBytes, cfg.Memory.Ratio, memlimit.Overrides{
		VectorCache: cfg.Memory.VectorCacheBytes,
	})
	budget.Apply()
	memlimit.Set(budget)

	slog.Info("memory budget",
		"limit", memlimit.Bytes(budget.Total), "source", budget.Source,
		"go_soft_limit", memlimit.Bytes(budget.GoLimit),
		"db_block_cache", memlimit.Bytes(budget.BlockCache),
		"db_memtable", memlimit.Bytes(budget.MemTable),
		"vector_cache", memlimit.Bytes(budget.VectorCache),
		"graph_cache", memlimit.Bytes(budget.GraphCache),
	)

	// Telemetry first so everything downstream is observable.
	collector, err := tell.New(ctx, cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("init telemetry; %w", err)
	}
	defer collector.Shutdown()

	// State database + registry.
	db, err := storage.Open(cfg.StateDir())
	if err != nil {
		return err
	}

	defer db.Close()

	reg, err := registry.New(db)
	if err != nil {
		return err
	}

	codeText, err := coderag.NewTextStore(db)
	if err != nil {
		return err
	}
	docsText, err := rag.NewTextStore(db)
	if err != nil {
		return err
	}

	// Durable work queue: queued (and interrupted running) background tasks
	// survive a restart instead of being lost with the process.
	taskStore, err := taskstore.New(db)
	if err != nil {
		return err
	}

	// graphify CLI + python discovery.
	gfy, err := graphify.New(cfg.Graphify.Bin, cfg.Graphify.Python, cfg.Graphify.BuildTimeout, cfg.Graphify.Exclude)
	if err != nil {
		return err
	}

	slog.Info("graphify resolved", "version", gfy.Version(), "tested_version", graphify.TestedVersion,
		"python", gfy.Python())

	// Native in-process graph query engine (replaces the python serve pool).
	// Bounded by an estimated-memory budget so tracking many repos cannot pin
	// every parsed graph in RAM and OOM-kill the process. An unset cap follows
	// the process budget; a negative one opts out of eviction entirely.
	engine := graphquery.NewEngine(graphCacheBytes(cfg.Graphify.CacheMaxBytes, budget))

	// Per-host SSH/token credentials are managed in the persisted credential
	// store through the UI/REST API; there is no global file-config fallback.
	git := gitops.New("")

	creds, err := credentials.New(db, cfg.KeysDir())
	if err != nil {
		return err
	}

	// Runtime-mutable workload settings. Safe defaults are persisted on first
	// run; thereafter the UI/REST/MCP-managed record is authoritative.
	settingsStore, err := settings.New(db, settings.Defaults())
	if err != nil {
		return err
	}

	mgr := manager.New(ctx, reg, git, gfy, engine, creds, codeText, cfg.ReposDir(), cfg.MergedGraphPath(),
		cfg.Graphify.Merge,
		manager.DocsDeps{
			TextStore:      docsText,
			DocsRootDir:    cfg.DocsRootDir(),
			DocsVectorsDir: cfg.DocsVectorsDir(),
			CodeVectorsDir: cfg.CodeVectorsDir(),
			SourcesRootDir: cfg.SourcesRootDir(),
		},
	)
	defer func() {
		if err := mgr.Close(); err != nil {
			slog.Error("close manager", "error", err)
		}
	}()
	mgr.SetSettingsStore(settingsStore)
	// Wire the durable task store into the queue before anything enqueues work,
	// so every submitted task is recorded and can be replayed after a restart.
	mgr.SetTaskStore(taskStore)

	// Web content sources (wikis, Confluence spaces). Each collection type has
	// a fetcher; new source types plug in here.
	webStore, err := websource.New(db)
	if err != nil {
		return err
	}
	mgr.SetWebSources(webStore, map[string]websource.Fetcher{
		websource.TypePages:      pages.New(pageCredentials(creds)),
		websource.TypeConfluence: confluence.New(),
		websource.TypeJira:       jira.New(),
	})
	if err := mgr.ReconcileInterruptedStages(ctx); err != nil {
		slog.Error("reconcile interrupted generation stages", "error", err)
	}
	// Rebuild graphs that predate managed ignores or were generated by a
	// different Graphify release. Jobs run through the bounded background queue.
	mgr.BackfillGraphs(ctx)
	// Drop a stale merged graph if cross-repo merging is now disabled.
	mgr.CleanupMergedGraph()
	if err := mgr.MigrateDocs(ctx); err != nil {
		slog.Error("migrate generated docs out of repository clones", "error", err)
	}

	// Build the initial docs/RAG client bundle from the persisted settings and
	// apply the work-queue concurrency limit. A build error here disables the
	// docs feature but does not abort startup.
	if s, gerr := settingsStore.Get(ctx); gerr != nil {
		slog.Error("load docs settings", "error", gerr)
	} else {
		mgr.SetTaskConcurrency(s.TaskConcurrency)
		if cerr := mgr.Configure(ctx, s); cerr != nil {
			slog.Error("configure docs/rag (disabled until fixed via settings)", "error", cerr)
		}
	}
	// Repos tracked before full-path ids used the last two URL segments as id,
	// which let repos from different (nested) groups collide; re-key them.
	// Runs after Configure so stale vector entries can be dropped.
	if err := mgr.MigrateRepoIDs(ctx); err != nil {
		slog.Error("migrate legacy repo ids", "error", err)
	}
	// Warm the normal (full-text) code index in the background so the server
	// starts listening immediately instead of waiting for every un-indexed repo
	// to be re-read and chunked. Repos are marked pending first, so a search
	// that arrives before the pass finishes warms just the repos it needs on
	// demand (see Manager.SearchCodeText) and never returns partial results.
	//
	// The two passes run one after the other in a single goroutine rather than
	// concurrently: each walks the whole corpus and buffers chunks, so running
	// them together doubles peak memory during the most fragile moment of the
	// process's life while halving nothing that matters — neither pass is on
	// the critical path for serving requests.
	go func() {
		if err := mgr.WarmCodeSearch(ctx); err != nil {
			slog.Error("warm normal code search index", "error", err)
		}
		if err := mgr.WarmDocsSearch(ctx); err != nil {
			slog.Error("warm lexical docs search index", "error", err)
		}
	}()

	// Re-enqueue background tasks that were queued (or running) when the
	// process last stopped. Done after the docs/RAG bundle is configured so a
	// restored refresh/generate can run its index stages, and after the
	// concurrency limit is applied so the backlog drains under the same bound.
	if err := mgr.RestoreTasks(ctx); err != nil {
		slog.Error("restore persisted background tasks", "error", err)
	}

	// Background poller. Repo cadence and per-source intervals are read from
	// persisted runtime settings, so changes apply without a restart.
	go scheduler.Run(ctx, mgr)

	// The Langfuse tracer is owned by the docs bundle and rebuilt on settings
	// changes; flush whichever one is live when the process stops, so the last
	// build's spans are not lost.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), langfuseShutdownTimeout)
		defer cancel()

		if err := mgr.Tracer().Shutdown(shutdownCtx); err != nil {
			slog.Warn("shutdown langfuse tracer", "error", err)
		}
	}()

	mcpServer := mcptools.New(mgr, version, cfg.MCP.WaitTimeout, mcptools.ToolProfileStandard)
	mcpFullServer := mcptools.New(mgr, version, cfg.MCP.WaitTimeout, mcptools.ToolProfileFull)

	// Server blocks until ctx is cancelled, then shuts down.
	if err := server.Start(ctx, cfg, mgr, mcpServer, mcpFullServer); err != nil {
		return fmt.Errorf("start server; %w", err)
	}

	return nil
}

// langfuseShutdownTimeout bounds the final flush of exported LLM traces on
// shutdown. Spans that cannot be shipped in that window are dropped rather
// than holding the process open.
const langfuseShutdownTimeout = 5 * time.Second

// graphCacheBytes resolves the parsed-graph cache cap. Zero (unset) follows the
// process memory budget so the cache stays proportional to the container limit;
// a negative value opts out of eviction, which the query engine expresses as 0.
func graphCacheBytes(configured int64, budget memlimit.Budget) int64 {
	switch {
	case configured > 0:
		return configured
	case configured < 0:
		return 0
	default:
		return budget.GraphCache
	}
}

// pageCredentials adapts the git credential store to web-page fetching: a
// stored pattern matching the page URL supplies basic-auth or bearer-token
// material for private wikis.
func pageCredentials(creds *credentials.Store) pages.CredentialFunc {
	return func(ctx context.Context, pageURL string) (string, string, error) {
		auth, err := creds.Resolve(ctx, pageURL)
		if err != nil || auth == nil {
			return "", "", err
		}

		return auth.Username, auth.Token, nil
	}
}
