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
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/mcptools"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/scheduler"
	"github.com/rytsh/krabby/internal/service/servepool"
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

	// Per-graph python MCP server pool.
	pool := servepool.New(ctx, gfy.Python(), version, cfg.Graphify.ServeIdleTimeout)
	defer pool.StopAll()

	git := gitops.New(cfg.Git.SSHKeyPath)

	creds, err := credentials.New(db, cfg.KeysDir())
	if err != nil {
		return err
	}

	mgr := manager.New(ctx, reg, git, gfy, pool, creds, cfg.ReposDir(), cfg.MergedGraphPath())

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
