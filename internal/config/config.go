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
	// Commit is the short git commit hash, injected at build time.
	Commit = "-"
	// Date is the UTC build timestamp, injected at build time.
	Date = "-"
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
	Memory   Memory   `cfg:"memory"`

	Telemetry tell.Config `cfg:"telemetry"`
}

// Memory bounds the process footprint. krabby holds three embedded Badger
// databases, a parsed-graph cache and transient indexing buffers, and shells
// out to the graphify CLI inside the same container; left untuned those add up
// to more than a typical few-gigabyte limit and the container is OOM-killed.
// Every derived cache size comes from the single limit below.
type Memory struct {
	// LimitBytes is the total memory budget. 0 auto-detects the cgroup
	// (container) limit, falling back to total system memory, which is the
	// right answer in almost every deployment; set it explicitly only when
	// krabby must share its cgroup with another workload.
	LimitBytes int64 `cfg:"limit_bytes"`
	// Ratio is the fraction of LimitBytes handed to the Go runtime as its soft
	// heap limit (GOMEMLIMIT). The remainder covers the graphify subprocess,
	// git, mmap'd database tables and runtime overhead outside the heap. An
	// explicit GOMEMLIMIT environment variable overrides this.
	Ratio float64 `cfg:"ratio" default:"0.75"`
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
	// CacheMaxBytes caps the estimated in-memory size of parsed graphs held by
	// the query engine. Each tracked repo's graph.json is parsed once and cached;
	// without a cap every graph stays resident forever, so tracking many repos
	// drives RSS up until the container OOM-kills. When the budget is exceeded
	// the least-recently-used graphs are evicted (and transparently reloaded on
	// the next query).
	//
	// 0 (the default) derives the cap from the process memory budget, which
	// keeps it proportional to the container limit instead of pinning a fixed
	// 512 MiB of a 4 GiB allowance. A negative value disables eviction
	// entirely (unbounded, the original behaviour).
	CacheMaxBytes int64 `cfg:"cache_max_bytes"`
}

// The structs below are no longer part of the file/env configuration: they
// are plain parameter carriers for the internal clients (llm, embedder, rag,
// docgen, coderag), populated from the runtime settings store.

// Filters select which files of a repository are indexed or documented. The
// same shape is used at two levels: the install-wide settings, and a
// per-repository override stored on the repo record.
//
// The rules are deliberately identical at both levels:
//
//   - Include, when non-empty, REPLACES the built-in allowlist. Use it to say
//     "in this repo, index only these".
//   - IncludeExtra is always ADDED on top of whatever Include resolved to
//     (built-in allowlist or explicit Include). Use it to say "also index
//     these", e.g. every YAML in a deployment repository, without having to
//     restate the whole allowlist.
//   - Exclude is applied last and wins over both.
//
// A repository override does not silently discard the install-wide settings:
// see Filters.Merge for how the two combine.
type Filters struct {
	Include      []string `cfg:"include"`
	IncludeExtra []string `cfg:"include_extra"`
	Exclude      []string `cfg:"exclude"`
}

// Merge overlays a per-repository override on install-wide filters.
//
// Include is a replacement, so the repo's own list wins outright when it sets
// one — that is the whole point of a per-repo Include, and unioning would make
// "only these files, in this repo" unexpressible. IncludeExtra and Exclude are
// additive: both only ever widen or narrow the set further, so combining them
// keeps an install-wide rule (say, "never index generated protobufs") in force
// no matter what a single repository asks for.
func (f Filters) Merge(over Filters) Filters {
	out := Filters{
		Include:      f.Include,
		IncludeExtra: concatGlobs(f.IncludeExtra, over.IncludeExtra),
		Exclude:      concatGlobs(f.Exclude, over.Exclude),
	}
	if len(over.Include) > 0 {
		out.Include = over.Include
	}

	return out
}

// Empty reports whether no filter is set at all.
func (f Filters) Empty() bool {
	return len(f.Include) == 0 && len(f.IncludeExtra) == 0 && len(f.Exclude) == 0
}

// concatGlobs joins two glob lists without aliasing either input, so a merged
// result can never be appended into one of the sources it was built from.
func concatGlobs(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}

	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)

	return append(out, b...)
}

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
	// Filters select which repo files are documented. See Filters.
	Filters
	// Prompt is the system prompt for the final synthesis of the comprehensive
	// repository documentation. Empty falls back to docgen.DefaultPrompt. The
	// per-file summaries and graph overview are appended as the user message.
	Prompt string `cfg:"prompt"`
	// PromptExtra is appended to whatever Prompt resolved to, rather than
	// replacing it. It is how an install-wide house rule ("always list config
	// keys in a table") is added without restating docgen.DefaultPrompt, which
	// carries constraints — mermaid escaping, required sections — that a
	// replacement silently drops.
	PromptExtra string `cfg:"prompt_extra"`
}

// DocsOverride is a per-repository override of the documentation settings. It
// follows the same replace/extend split as Filters: Prompt replaces the
// effective system prompt for this repository, PromptExtra is appended to it.
//
// Prefer PromptExtra. It is what expresses "in this repo, also do X" — the
// common case — while keeping the default prompt's rules in force.
type DocsOverride struct {
	Filters
	Prompt      string
	PromptExtra string
}

// Empty reports whether the override carries nothing at all.
func (o DocsOverride) Empty() bool {
	return o.Filters.Empty() && o.Prompt == "" && o.PromptExtra == ""
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
	// Dim is the requested output dimension, sent as the OpenAI "dimensions"
	// parameter; 0 leaves the model at its native width. It is worth setting
	// on a Matryoshka-trained model (Gemini Embedding 2 accepts 128-3072,
	// text-embedding-3 likewise): accuracy holds well below the default width
	// while the vector cache, which is live memory GOMEMLIMIT cannot reclaim,
	// scales linearly with it. Endpoints that reject the parameter fall back
	// to their native width automatically.
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
	// TopDocs is how many ranked document excerpts to return after grouping.
	TopDocs int `cfg:"top_docs" default:"3"`

	// HybridCandidates is how many documents each ranker contributes to hybrid
	// rank fusion. Both the semantic and the lexical arm are asked for this
	// depth; an asymmetric depth silently biases fusion toward the longer list.
	HybridCandidates int `cfg:"hybrid_candidates" default:"12"`
	// HybridRRFK is the reciprocal-rank-fusion smoothing constant. The classic
	// value of 60 was tuned for thousand-deep TREC runs and flattens a list
	// this short to a ~18% score spread, so a much smaller k is used here.
	HybridRRFK int `cfg:"hybrid_rrf_k" default:"20"`
	// HybridWeightLexical scales the BM25 arm's fused contribution.
	HybridWeightLexical float64 `cfg:"hybrid_weight_lexical" default:"1"`
	// HybridWeightSemantic scales the embedding arm's fused contribution.
	HybridWeightSemantic float64 `cfg:"hybrid_weight_semantic" default:"1"`

	// LexicalStopWords are dropped from a question before it is turned into a
	// BM25 query. It is empty by default and there is no built-in list: BM25's
	// IDF already gives a term that appears in most documents a near-zero
	// score in any language, so this is a latency knob, not a relevance one.
	// Set it to your corpus language's function words on a large corpus.
	LexicalStopWords []string `cfg:"lexical_stop_words"`
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
	// Filters select which repo files are indexed. Empty uses the built-in
	// allowlist (source extensions, build manifests and deploy config). See
	// Filters: a non-empty Include replaces that allowlist and also disables
	// the default node_modules/vendor skip, whereas IncludeExtra adds to it.
	Filters
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
