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
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/mcptools"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/scheduler"
	"github.com/rytsh/krabby/internal/service/settings"
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

	// graphify CLI + python discovery.
	gfy, err := graphify.New(cfg.Graphify.Bin, cfg.Graphify.Python, cfg.Graphify.BuildTimeout)
	if err != nil {
		return err
	}

	slog.Info("graphify resolved", "python", gfy.Python())

	// Native in-process graph query engine (replaces the python serve pool).
	engine := graphquery.NewEngine()

	git := gitops.New(cfg.Git.SSHKeyPath)

	creds, err := credentials.New(db, cfg.KeysDir())
	if err != nil {
		return err
	}

	// Runtime-mutable docs/RAG settings, seeded from file/env config on first run
	// and thereafter configurable live via MCP tools / the UI.
	settingsStore, err := settings.New(db, seedSettings(cfg))
	if err != nil {
		return err
	}

	mgr := manager.New(ctx, reg, git, gfy, engine, creds, cfg.ReposDir(), cfg.MergedGraphPath(),
		manager.DocsDeps{
			DocsDir:    cfg.DocsDir,
			VectorsDir: cfg.VectorsDir(),
		},
	)
	mgr.SetSettingsStore(settingsStore)

	// Build the initial docs/RAG client bundle from the persisted settings.
	// A build error here disables the feature but does not abort startup.
	if s, gerr := settingsStore.Get(ctx); gerr != nil {
		slog.Error("load docs settings", "error", gerr)
	} else if cerr := mgr.Configure(ctx, s); cerr != nil {
		slog.Error("configure docs/rag (disabled until fixed via settings)", "error", cerr)
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

	// Background poller.
	go scheduler.Run(ctx, mgr, cfg.Git.PollInterval)

	mcpServer := mcptools.New(mgr, version)

	// Server blocks until ctx is cancelled, then shuts down.
	if err := server.Start(ctx, cfg, mgr, mcpServer); err != nil {
		return fmt.Errorf("start server; %w", err)
	}

	return nil
}

// seedSettings converts file/env config into the initial persisted docs/RAG
// settings. It is only used the first time krabby runs against a fresh state DB;
// afterwards the persisted record (editable via the UI/MCP) is authoritative.
func seedSettings(cfg *config.Config) settings.Settings {
	return settings.Settings{
		DocsEnabled:     cfg.Docs.Enabled,
		DocsConcurrency: cfg.Docs.Concurrency,
		DocsInclude:     cfg.Docs.Include,
		DocsExclude:     cfg.Docs.Exclude,
		DocsPrompt:      cfg.Docs.Prompt,

		LLMBaseURL: cfg.LLM.BaseURL,
		LLMAPIKey:  cfg.LLM.APIKey,
		LLMModel:   cfg.LLM.Model,
		LLMTimeout: cfg.LLM.Timeout,

		EmbedBaseURL: cfg.Embedder.BaseURL,
		EmbedAPIKey:  cfg.Embedder.APIKey,
		EmbedModel:   cfg.Embedder.Model,
		EmbedDim:     cfg.Embedder.Dim,
		EmbedBatch:   cfg.Embedder.Batch,
		EmbedTimeout: cfg.Embedder.Timeout,

		RAGEnabled:      cfg.RAG.Enabled,
		RAGChunkSize:    cfg.RAG.ChunkSize,
		RAGChunkOverlap: cfg.RAG.ChunkOverlap,
		RAGTopK:         cfg.RAG.TopK,
		RAGTopDocs:      cfg.RAG.TopDocs,
		StoreKind:       cfg.RAG.Store.Kind,

		QdrantURL:        cfg.RAG.Store.Qdrant.URL,
		QdrantAPIKey:     cfg.RAG.Store.Qdrant.APIKey,
		QdrantCollection: cfg.RAG.Store.Qdrant.Collection,
	}
}
