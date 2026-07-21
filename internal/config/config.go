// Package config loads krabby's layered configuration via chu.
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rakunlabs/chu"
	"github.com/rakunlabs/chu/loader/loaderenv"
	"github.com/rakunlabs/logi"
	"github.com/rakunlabs/tell"
)

var (
	// ServiceName is the application name used for config discovery and banners.
	ServiceName = "krabby"
	// Version is injected at build time.
	Version = "v0.0.0"
)

// Config is the root configuration for krabby.
type Config struct {
	LogLevel string `cfg:"log_level" default:"info"`
	// DataDir holds clones, merged graph and registry state. "~" is expanded.
	DataDir string `cfg:"data_dir" default:"~/.krabby"`

	Server   Server   `cfg:"server"`
	MCP      MCP      `cfg:"mcp"`
	Git      Git      `cfg:"git"`
	Graphify Graphify `cfg:"graphify"`
	Webhook  Webhook  `cfg:"webhook"`

	// Repos seeds the registry at startup; repos can also be added at runtime.
	Repos []RepoSeed `cfg:"repos"`

	Telemetry tell.Config `cfg:"telemetry"`
}

// Server is the HTTP listen configuration.
type Server struct {
	Host string `cfg:"host"`
	Port string `cfg:"port" default:"8080"`
}

// MCP configures the model-context-protocol endpoint.
type MCP struct {
	Path   string `cfg:"path" default:"/mcp"`
	APIKey string `cfg:"api_key" log:"-"`
}

// Git configures repository access and background polling.
type Git struct {
	// SSHKeyPath, when set, is used via GIT_SSH_COMMAND for private repos.
	SSHKeyPath string `cfg:"ssh_key_path"`
	// PollInterval is how often the scheduler checks remotes for new commits.
	PollInterval time.Duration `cfg:"poll_interval" default:"1h"`
}

// Graphify configures the graphify CLI integration.
type Graphify struct {
	// Bin is the graphify CLI binary (PATH lookup allowed).
	Bin string `cfg:"bin" default:"graphify"`
	// Python is the interpreter that can `import graphify`. Empty = derive
	// from the graphify binary shebang, falling back to python3.
	Python string `cfg:"python"`
	// BuildTimeout bounds a single extract/update/merge run.
	BuildTimeout time.Duration `cfg:"build_timeout" default:"30m"`
	// ServeIdleTimeout kills per-graph python MCP servers idle this long. 0 disables.
	ServeIdleTimeout time.Duration `cfg:"serve_idle_timeout" default:"30m"`
}

// Webhook configures inbound webhook verification.
type Webhook struct {
	// GithubSecret verifies X-Hub-Signature-256 on /webhook/github. Empty disables verification.
	GithubSecret string `cfg:"github_secret" log:"-"`
}

// RepoSeed is a repository declared in the config file.
type RepoSeed struct {
	URL    string `cfg:"url"`
	Branch string `cfg:"branch"`
}

// Load reads configuration (default -> file -> env) and initializes log level.
func Load(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := chu.Load(ctx, ServiceName, &cfg,
		chu.WithLoaderOption(loaderenv.New(
			loaderenv.WithPrefix("KRABBY_"),
		)),
		chu.WithVersion(Version),
	); err != nil {
		return nil, fmt.Errorf("load config; %w", err)
	}

	if err := logi.SetLogLevel(cfg.LogLevel); err != nil {
		return nil, fmt.Errorf("set log level %s; %w", cfg.LogLevel, err)
	}

	dir, err := expandHome(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("expand data_dir; %w", err)
	}
	cfg.DataDir = dir

	slog.Info("loaded configuration", "config", chu.MarshalMap(cfg))

	return &cfg, nil
}

// ReposDir is where repositories are cloned.
func (c *Config) ReposDir() string { return filepath.Join(c.DataDir, "repos") }

// MergedGraphPath is the cross-repo merged graph location.
func (c *Config) MergedGraphPath() string {
	return filepath.Join(c.DataDir, "merged", "graph.json")
}

// StateDir is the registry database location.
func (c *Config) StateDir() string { return filepath.Join(c.DataDir, "state") }

// KeysDir holds materialized SSH key files for stored credentials.
func (c *Config) KeysDir() string { return filepath.Join(c.DataDir, "keys") }

func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}

		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}

	return p, nil
}
