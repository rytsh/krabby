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

	// Docs configures LLM-generated markdown documentation per repo.
	Docs Docs `cfg:"docs"`
	// LLM is the OpenAI-compatible chat client used by docgen and ask_docs.
	LLM LLM `cfg:"llm"`
	// Embedder is the OpenAI-compatible embeddings client used by RAG.
	Embedder Embedder `cfg:"embedder"`
	// RAG configures chunking, retrieval and the vector store backend.
	RAG RAG `cfg:"rag"`

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

// Docs configures the repo -> markdown documentation generator.
type Docs struct {
	// Enabled turns on doc generation in the refresh pipeline. When false,
	// no docs are generated even if an LLM is configured.
	Enabled bool `cfg:"enabled"`
	// Concurrency bounds parallel per-file LLM doc calls.
	Concurrency int `cfg:"concurrency" default:"4"`
	// Include globs select source files to document (repo-relative).
	Include []string `cfg:"include"`
	// Exclude globs skip files (evaluated after Include).
	Exclude []string `cfg:"exclude"`
}

// LLM configures an OpenAI-compatible chat-completions endpoint.
type LLM struct {
	// BaseURL is the API root, e.g. "https://api.openai.com/v1".
	BaseURL string `cfg:"base_url"`
	// APIKey is sent as a Bearer token. Empty is allowed for local servers.
	APIKey string `cfg:"api_key" log:"-"`
	// Model is the chat model name.
	Model string `cfg:"model" default:"gpt-4o-mini"`
	// Timeout bounds a single completion request.
	Timeout time.Duration `cfg:"timeout" default:"60s"`
}

// Embedder configures an OpenAI-compatible embeddings endpoint.
type Embedder struct {
	// BaseURL is the API root, e.g. "http://localhost:11434/v1" (Ollama).
	BaseURL string `cfg:"base_url"`
	// APIKey is sent as a Bearer token. Empty is allowed for local servers.
	APIKey string `cfg:"api_key" log:"-"`
	// Model is the embedding model name.
	Model string `cfg:"model"`
	// Dim is the expected embedding dimension; 0 = infer from first response.
	Dim int `cfg:"dim"`
	// Batch bounds how many inputs are sent per embeddings request.
	Batch int `cfg:"batch" default:"64"`
	// Timeout bounds a single embeddings request.
	Timeout time.Duration `cfg:"timeout" default:"30s"`
}

// RAG configures chunking, retrieval and the vector store backend.
type RAG struct {
	// Enabled turns on indexing + retrieval in the pipeline and tools.
	Enabled bool `cfg:"enabled"`
	// ChunkSize is the target chunk length in characters.
	ChunkSize int `cfg:"chunk_size" default:"1200"`
	// ChunkOverlap is the character overlap between adjacent chunks.
	ChunkOverlap int `cfg:"chunk_overlap" default:"200"`
	// TopK is how many chunk matches to fetch before grouping into docs.
	TopK int `cfg:"top_k" default:"20"`
	// TopDocs is how many whole documents to return after grouping.
	TopDocs int `cfg:"top_docs" default:"5"`
	// Store selects and configures the vector store backend.
	Store VectorStore `cfg:"store"`
}

// VectorStore selects a vector backend and holds per-backend settings.
type VectorStore struct {
	// Kind is "embedded" (default, file-backed) or "qdrant".
	Kind string `cfg:"kind" default:"embedded"`
	// Qdrant settings apply when Kind == "qdrant".
	Qdrant Qdrant `cfg:"qdrant"`
}

// Qdrant configures the Qdrant HTTP backend.
type Qdrant struct {
	URL        string `cfg:"url" default:"http://localhost:6333"`
	APIKey     string `cfg:"api_key" log:"-"`
	Collection string `cfg:"collection" default:"krabby"`
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

// DocsDir is where generated markdown docs live for a given repo clone path.
func (c *Config) DocsDir(repoPath string) string {
	return filepath.Join(repoPath, "krabby-docs")
}

// VectorsDir holds the embedded vector store backend's data.
func (c *Config) VectorsDir() string { return filepath.Join(c.DataDir, "vectors") }

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
