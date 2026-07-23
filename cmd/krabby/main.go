// Command krabby serves multi-repo graphify knowledge graphs over MCP.
package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rakunlabs/into"
	"github.com/rakunlabs/logi"
	"github.com/rakunlabs/tell"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/server"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/mcptools"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/scheduler"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/confluence"
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

	// graphify CLI + python discovery.
	gfy, err := graphify.New(cfg.Graphify.Bin, cfg.Graphify.Python, cfg.Graphify.BuildTimeout, cfg.Graphify.Exclude)
	if err != nil {
		return err
	}

	slog.Info("graphify resolved", "python", gfy.Python())

	// Native in-process graph query engine (replaces the python serve pool).
	// Bounded by an estimated-memory budget so tracking many repos cannot pin
	// every parsed graph in RAM and OOM-kill the process.
	engine := graphquery.NewEngine(cfg.Graphify.CacheMaxBytes)

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

	// Web content sources (wikis, Confluence spaces). Each collection type has
	// a fetcher; new source types plug in here.
	webStore, err := websource.New(db)
	if err != nil {
		return err
	}
	mgr.SetWebSources(webStore, map[string]websource.Fetcher{
		websource.TypePages:      pages.New(pageCredentials(creds)),
		websource.TypeConfluence: confluence.New(),
	})
	if err := mgr.ReconcileInterruptedStages(ctx); err != nil {
		slog.Error("reconcile interrupted generation stages", "error", err)
	}
	// Repos tracked before the .graphifyignore feature keep stale testdata /
	// fixture nodes until rebuilt; backfill the ignore file and rebuild them.
	mgr.BackfillGraphIgnore(ctx)
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
	if err := mgr.WarmCodeSearch(ctx); err != nil {
		slog.Error("warm normal code search index", "error", err)
	}

	// Seed repos from config; builds run in the background.
	for _, seed := range cfg.Repos {
		if seed.URL == "" {
			continue
		}

		if _, err := mgr.AddRepo(ctx, seed.URL, seed.Branch); err != nil {
			slog.Error("seed repo", "url", seed.URL, "error", err)
		}
	}

	// Background poller. Repo cadence and per-source intervals are read from
	// persisted runtime settings, so changes apply without a restart.
	go scheduler.Run(ctx, mgr)

	mcpServer := mcptools.New(mgr, version, cfg.MCP.WaitTimeout, mcptools.ToolProfileStandard)
	mcpFullServer := mcptools.New(mgr, version, cfg.MCP.WaitTimeout, mcptools.ToolProfileFull)

	// Server blocks until ctx is cancelled, then shuts down.
	if err := server.Start(ctx, cfg, mgr, mcpServer, mcpFullServer); err != nil {
		return fmt.Errorf("start server; %w", err)
	}

	return nil
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
