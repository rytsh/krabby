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
	_ "github.com/rakunlabs/chu/loader/external/loaderconsul"
	_ "github.com/rakunlabs/chu/loader/external/loadervault"
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

// Config is the root file/env configuration for krabby. It intentionally
// carries only the system-level settings that require a restart (listen
// address, data directory, tool paths). Everything workload-related — docs
// generation, LLM/embedder endpoints, RAG tuning, git polling, webhook
// verification — is runtime-mutable and lives in the persisted settings
// store, managed via the UI and REST API (see internal/service/settings).
type Config struct {
	LogLevel string `cfg:"log_level" default:"info"`
	// DataDir holds clones, merged graph and registry state. "~" is expanded.
	DataDir string `cfg:"data_dir" default:"~/.krabby"`

	Server   Server   `cfg:"server"`
	MCP      MCP      `cfg:"mcp"`
	Graphify Graphify `cfg:"graphify"`

	// Repos seeds the registry at startup; repos can also be added at runtime.
	Repos []RepoSeed `cfg:"repos"`

	Telemetry tell.Config `cfg:"telemetry"`
}

// Server is the HTTP listen configuration.
type Server struct {
	Host string `cfg:"host"`
	Port string `cfg:"port" default:"8080"`
	// BasePath serves the whole app (UI, REST API, MCP, webhook) under a URL
	// prefix, e.g. "/krabby" when running behind a reverse proxy on a subpath.
	// It is normalized to a leading slash with no trailing slash; empty (the
	// default) serves everything at the root.
	BasePath string `cfg:"base_path"`
}

// MCP configures the model-context-protocol endpoint.
type MCP struct {
	Path   string `cfg:"path" default:"/mcp"`
	APIKey string `cfg:"api_key" log:"-"`
	// WaitTimeout caps how long wait=true add_repo/refresh_repo calls block
	// before returning the in-progress status. The build keeps running in the
	// background either way; poll repo_status for the final state. 0 waits
	// until the build finishes or the client cancels.
	WaitTimeout time.Duration `cfg:"wait_timeout" default:"300s"`
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
	// Exclude lists extra gitignore-style patterns krabby writes into a managed
	// section of the clone's .graphifyignore before each build, so the graph
	// skips test fixtures and other non-architectural noise. These are appended
	// to DefaultGraphIgnore; leave empty to use the defaults alone.
	Exclude []string `cfg:"exclude"`
	// Merge builds a cross-repo merged graph (queried when a graph tool is
	// called with no repo). It only adds value when tracked repos share symbols
	// directly (a split monorepo, interdependent modules); for independent
	// services it is a disjoint union with no cross-repo edges, so it defaults
	// off to avoid the rebuild cost. When off, graph tools require a repo id.
	Merge bool `cfg:"merge"`
}

// The structs below are no longer part of the file/env configuration: they
// are plain parameter carriers for the internal clients (llm, embedder, rag,
// docgen, coderag), populated from the runtime settings store.

// Docs configures the repo -> markdown documentation generator.
type Docs struct {
	// Enabled turns on doc generation in the refresh pipeline. When false,
	// no docs are generated even if an LLM is configured.
	Enabled bool `cfg:"enabled"`
	// Concurrency bounds parallel per-file LLM summary calls.
	Concurrency int `cfg:"concurrency" default:"8"`
	// SummaryModel is the chat model used for the per-file summary phase (the
	// bulk of the calls). It is dense factual extraction, so a fast, cheap model
	// (e.g. gemini-2.5-flash) is a good fit and much faster than a reasoning
	// model. Empty falls back to the main LLM model. It reuses the main LLM's
	// base URL, API key and timeout; only the model name differs.
	SummaryModel string `cfg:"summary_model"`
	// MaxGroups caps how many grouped summary LLM calls a single run makes.
	// Files are clustered by graphify community; when a repo has more
	// communities than this, small communities are packed together so the call
	// count stays bounded regardless of how fragmented the graph is. 0 uses the
	// built-in default.
	MaxGroups int `cfg:"max_groups" default:"40"`
	// Include globs select source files to document (repo-relative).
	Include []string `cfg:"include"`
	// Exclude globs skip files (evaluated after Include).
	Exclude []string `cfg:"exclude"`
	// Prompt is the system prompt for the final synthesis of the comprehensive
	// repository documentation. Empty falls back to docgen.DefaultPrompt. The
	// per-file summaries and graph overview are appended as the user message.
	Prompt string `cfg:"prompt"`
}

// LLM configures an OpenAI-compatible chat-completions endpoint.
type LLM struct {
	// BaseURL is the API root, e.g. "https://api.openai.com/v1".
	BaseURL string `cfg:"base_url"`
	// APIKey is sent as a Bearer token. Empty is allowed for local servers.
	APIKey string `cfg:"api_key" log:"-"`
	// Model is the chat model name.
	Model string `cfg:"model" default:"gpt-4o-mini"`
	// Timeout bounds a single completion request. Large synthesis calls can
	// take minutes, so keep this generous.
	Timeout time.Duration `cfg:"timeout" default:"300s"`
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
	// Concurrency bounds how many embedding batch requests run in parallel.
	Concurrency int `cfg:"concurrency" default:"4"`
	// Timeout bounds a single embeddings request.
	Timeout time.Duration `cfg:"timeout" default:"30s"`
}

// RAG configures chunking and retrieval over the embedded vector store.
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
}

// CodeRAG configures semantic search over raw source code. It indexes into a
// separate embedded store so docs and code can use different model dimensions.
type CodeRAG struct {
	// Enabled turns on semantic vector indexing. Normal search_code queries use
	// the always-available local bw full-text index.
	Enabled bool `cfg:"enabled"`
	// ChunkSize is the target chunk length in characters. The 3000/1000
	// defaults follow the Codestral Embed retrieval recommendation.
	ChunkSize int `cfg:"chunk_size" default:"3000"`
	// ChunkOverlap is the character overlap between adjacent chunks.
	ChunkOverlap int `cfg:"chunk_overlap" default:"1000"`
	// TopK is how many code snippets to return per search.
	TopK int `cfg:"top_k" default:"10"`
	// Include globs select source files to index (repo-relative). Empty uses a
	// built-in source-extension allowlist.
	Include []string `cfg:"include"`
	// Exclude globs skip files (evaluated after Include).
	Exclude []string `cfg:"exclude"`
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

	cfg.Server.BasePath = NormalizeBasePath(cfg.Server.BasePath)

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

// DocsRootDir holds generated markdown documentation outside repository clones.
func (c *Config) DocsRootDir() string { return filepath.Join(c.DataDir, "docs") }

// DocsVectorsDir holds the embedded vector store data for docs RAG.
func (c *Config) DocsVectorsDir() string { return filepath.Join(c.DataDir, "docs-vectors") }

// CodeVectorsDir holds the embedded vector store data for code RAG. It is a
// separate database from DocsVectorsDir because the two indexes may use
// embedding models with different dimensions (a dim change wipes the whole
// store).
func (c *Config) CodeVectorsDir() string { return filepath.Join(c.DataDir, "code-vectors") }

// SourcesRootDir holds synced web-source markdown by collection name.
func (c *Config) SourcesRootDir() string { return filepath.Join(c.DataDir, "sources") }

// NormalizeBasePath cleans a configured base path into a canonical form: either
// "" (serve at root) or "/segment[/segment...]" with a leading slash and no
// trailing slash. Whitespace and redundant slashes are collapsed.
func NormalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}

	// Collapse any interior duplicate slashes.
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' })

	return "/" + strings.Join(parts, "/")
}

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
