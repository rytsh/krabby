package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
)

// ErrDocsDisabled is returned by doc/RAG methods when the subsystem is off.
var ErrDocsDisabled = errors.New("docs/rag subsystem is not enabled")

// ErrCodeRAGDisabled is returned when source-code semantic search is off.
var ErrCodeRAGDisabled = errors.New("code rag subsystem is not enabled")

// ErrManagerClosed is returned when live configuration is attempted during
// shutdown.
var ErrManagerClosed = errors.New("manager is shutting down")

// ErrNoSettingsStore is returned when config methods are called before a
// settings store has been attached.
var ErrNoSettingsStore = errors.New("settings store not configured")

// SetSettingsStore attaches the persisted settings store. Called once at
// startup, before Configure.
func (m *Manager) SetSettingsStore(s *settings.Store) {
	m.settings = s
}

// InitMCPKey resolves the effective MCP API key at startup: a persisted
// runtime override wins over the file/env config value.
func (m *Manager) InitMCPKey(ctx context.Context, configKey string) {
	key := configKey

	if m.settings != nil {
		if rec, err := m.settings.MCPKey(ctx); err != nil {
			slog.Error("load mcp key override", "error", err)
		} else if rec != nil {
			key = rec.Key
		}
	}

	m.mcpKeyMu.Lock()
	m.mcpKey = key
	m.mcpConfigKey = configKey
	m.mcpKeyMu.Unlock()
}

// MCPAPIKey returns the currently effective MCP API key ("" = open endpoint).
func (m *Manager) MCPAPIKey() string {
	m.mcpKeyMu.RLock()
	defer m.mcpKeyMu.RUnlock()

	return m.mcpKey
}

// SetMCPAPIKey persists a runtime MCP key override and applies it immediately.
// An empty key disables authentication.
func (m *Manager) SetMCPAPIKey(ctx context.Context, key string) error {
	if m.settings == nil {
		return ErrNoSettingsStore
	}

	if err := m.settings.SetMCPKey(ctx, key); err != nil {
		return err
	}

	m.mcpKeyMu.Lock()
	m.mcpKey = key
	m.mcpKeyMu.Unlock()

	return nil
}

// ClearMCPAPIKey removes the runtime override; the file/env config value (as
// captured at startup) applies again.
func (m *Manager) ClearMCPAPIKey(ctx context.Context) error {
	if m.settings == nil {
		return ErrNoSettingsStore
	}

	if err := m.settings.ClearMCPKey(ctx); err != nil {
		return err
	}

	m.mcpKeyMu.Lock()
	m.mcpKey = m.mcpConfigKey
	m.mcpKeyMu.Unlock()

	return nil
}

// PollInterval returns the repo polling cadence from the runtime settings:
// the persisted value, one hour when unset, disabled (0) when negative.
func (m *Manager) PollInterval() time.Duration {
	const def = time.Hour

	if m.settings == nil {
		return def
	}

	s, err := m.settings.Get(context.Background())
	if err != nil {
		slog.Error("load poll interval", "error", err)

		return def
	}

	switch {
	case s.GitPollInterval < 0:
		return 0
	case s.GitPollInterval == 0:
		return def
	default:
		return s.GitPollInterval
	}
}

// RepoSchedules returns the effective repository poll schedules from the
// runtime settings: the configured per-namespace cron schedules, or a single
// fallback derived from GitPollInterval when none are configured. The scheduler
// reads this on every reconcile tick so UI/REST changes apply without a
// restart.
func (m *Manager) RepoSchedules() []settings.RepoSchedule {
	if m.settings == nil {
		return nil
	}

	s, err := m.settings.Get(context.Background())
	if err != nil {
		slog.Error("load repo schedules", "error", err)

		return nil
	}

	return s.EffectiveSchedules()
}

// RefreshNamespace queues a background refresh for every repo in ns (using the
// same namespace semantics as the registry: "" / "default" is the default
// bucket, "*" is every namespace). Called by the scheduler when a namespace's
// cron fires. Triggers coalesce per repo and the work queue bounds concurrency.
func (m *Manager) RefreshNamespace(ctx context.Context, ns string) error {
	repos, err := m.reg.ListNamespace(ctx, ns)
	if err != nil {
		return fmt.Errorf("list repos for namespace %q; %w", ns, err)
	}

	for _, repo := range repos {
		m.TriggerRefresh(repo.ID)
	}

	return nil
}

// WebhookSecret returns the provider-neutral git webhook verification secret from the
// runtime settings ("" disables signature verification).
func (m *Manager) WebhookSecret() string {
	if m.settings == nil {
		return ""
	}

	s, err := m.settings.Get(context.Background())
	if err != nil {
		slog.Error("load webhook secret", "error", err)

		return ""
	}

	return s.WebhookSecret
}

// GetDocsConfig returns the current docs/RAG settings with secrets redacted.
func (m *Manager) GetDocsConfig(ctx context.Context) (settings.Redacted, error) {
	if m.settings == nil {
		return settings.Redacted{}, ErrNoSettingsStore
	}

	s, err := m.settings.Get(ctx)
	if err != nil {
		return settings.Redacted{}, err
	}

	return redactSettings(s), nil
}

// SetDocsConfig persists a settings patch (empty secrets keep existing values),
// then rebuilds the docs/RAG clients live. If the rebuild fails the previous
// working bundle stays active and the error is returned; the settings are still
// persisted so the user can correct them.
func (m *Manager) SetDocsConfig(ctx context.Context, patch settings.Settings) (settings.Redacted, error) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()

	// A whole-record write cannot tell what changed, so it always reindexes.
	return m.setDocsConfig(ctx, patch, true)
}

// PatchDocsConfig atomically merges a presence-aware patch with persisted
// settings, then rebuilds clients. Concurrent patches cannot overwrite fields
// from a stale read.
func (m *Manager) PatchDocsConfig(ctx context.Context, patch settings.Patch) (settings.Redacted, error) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()

	if m.settings == nil {
		return settings.Redacted{}, ErrNoSettingsStore
	}

	current, err := m.settings.Get(ctx)
	if err != nil {
		return settings.Redacted{}, err
	}

	next := patch.Apply(current)
	if patch.RuntimeOnly() {
		saved, err := m.settings.Set(ctx, next)
		if err != nil {
			return settings.Redacted{}, err
		}

		// Runtime-only patches skip the client rebuild, so apply the queue
		// concurrency change here directly.
		m.SetTaskConcurrency(saved.TaskConcurrency)

		return redactSettings(saved), nil
	}

	// Observability changes rebuild the clients (the tracer is attached to
	// them) but touch nothing that went into a vector, so they must not drag
	// every repository through a reindex.
	return m.setDocsConfig(ctx, next, !patch.ObservabilityOnly())
}

// setDocsConfig persists next, rebuilds the clients, and optionally rebuilds
// the derived search indexes.
func (m *Manager) setDocsConfig(ctx context.Context, next settings.Settings, reindex bool) (settings.Redacted, error) {
	if m.settings == nil {
		return settings.Redacted{}, ErrNoSettingsStore
	}

	current, err := m.settings.Get(ctx)
	if err != nil {
		return settings.Redacted{}, err
	}
	imageChanged := webImageSettingsChanged(current, next)

	saved, err := m.settings.Set(ctx, next)
	if err != nil {
		return settings.Redacted{}, err
	}

	m.SetTaskConcurrency(saved.TaskConcurrency)

	if err := m.Configure(ctx, saved); err != nil {
		return redactSettings(saved), fmt.Errorf("settings saved but rebuild failed; %w", err)
	}

	// Existing repositories may be unchanged, so a normal refresh would return
	// before indexing. Rebuild derived docs/code indexes explicitly after live
	// settings changes (model, chunking, filters, or enablement).
	if reindex {
		m.TriggerReindexAll()
	}
	if imageChanged {
		m.triggerImageSourceRefreshes(ctx)
	}

	return redactSettings(saved), nil
}

func webImageSettingsChanged(a, b settings.Settings) bool {
	aVision, bVision := visionLLMConfig(a), visionLLMConfig(b)
	return a.WebImageAnalysisEnabled != b.WebImageAnalysisEnabled ||
		strings.TrimRight(aVision.BaseURL, "/") != strings.TrimRight(bVision.BaseURL, "/") ||
		aVision.Model != bVision.Model ||
		strings.TrimSpace(a.WebImageModel) != strings.TrimSpace(b.WebImageModel) ||
		a.EffectiveWebImageMaxPerPage() != b.EffectiveWebImageMaxPerPage() ||
		a.EffectiveWebImageMaxBytes() != b.EffectiveWebImageMaxBytes() ||
		a.EffectiveWebImageMaxPixels() != b.EffectiveWebImageMaxPixels() ||
		a.WebImageAllowAuthenticated != b.WebImageAllowAuthenticated
}

func (m *Manager) triggerImageSourceRefreshes(ctx context.Context) {
	if m.webStore == nil {
		return
	}
	collections, err := m.webStore.ListCollections(ctx)
	if err != nil {
		slog.Error("list web sources for image refresh", "error", err)
		return
	}
	for _, col := range collections {
		if col.AnalyzeImages {
			m.TriggerWebFullRefresh(col.Name)
		}
	}
}

func redactSettings(s settings.Settings) settings.Redacted {
	// Installs migrated to the task_concurrency setting have 0 stored; present
	// the effective default so the UI shows the value actually applied.
	if s.TaskConcurrency <= 0 {
		s.TaskConcurrency = queue.DefaultConcurrency
	}

	r := s.Redact()
	r.DocsDefaultPrompt = docgen.DefaultPrompt

	return r
}

// Configure builds a new docs/RAG client bundle from s and swaps it in
// atomically. On success the previous bundle's store is closed. On failure the
// previous (working) bundle is left in place and the error is returned so the
// caller (UI/MCP) can surface it.
//
// This is called once at startup with the persisted/seeded settings, and again
// on every settings update, giving live reconfiguration without a restart.
// tracerShutdownTimeout bounds the final flush of a replaced tracer. Spans
// that cannot be shipped in that window are dropped rather than delaying the
// reconfiguration.
const tracerShutdownTimeout = 5 * time.Second

func (m *Manager) Configure(_ context.Context, s settings.Settings) error {
	m.configureMu.Lock()
	defer m.configureMu.Unlock()

	m.lifecycleMu.Lock()
	closing := m.closing
	m.lifecycleMu.Unlock()
	if closing {
		return ErrManagerClosed
	}

	bundle, err := m.buildBundle(s)
	if err != nil {
		return err
	}

	m.docsMu.Lock()
	prev := m.docs
	m.docs = bundle
	m.docsMu.Unlock()

	// Close the stores owned by the replaced bundle (if any and distinct).
	if prev != nil && prev.store != nil && prev.store != bundle.store {
		if cerr := prev.store.Close(); cerr != nil {
			slog.Warn("close previous vector store", "error", cerr)
		}
	}

	if prev != nil && prev.codeStore != nil && prev.codeStore != bundle.codeStore {
		if cerr := prev.codeStore.Close(); cerr != nil {
			slog.Warn("close previous code vector store", "error", cerr)
		}
	}

	// The tracer is shut down only when the swap actually replaced it
	// (tracerFor hands the live one forward whenever the configuration is
	// unchanged). Shutdown flushes, so it is given its own bounded context
	// rather than the caller's, which may already be cancelled.
	if prev != nil && prev.tracer != nil && prev.tracer != bundle.tracer {
		go func(t *langfuse.Tracer) {
			ctx, cancel := context.WithTimeout(context.Background(), tracerShutdownTimeout)
			defer cancel()

			if cerr := t.Shutdown(ctx); cerr != nil {
				slog.Warn("shutdown previous langfuse tracer", "error", cerr)
			}
		}(prev.tracer)
	}

	slog.Info("docs/rag reconfigured",
		"docgen", bundle.gen != nil,
		"rag", bundle.rag != nil,
		"vision", bundle.vision != nil,
		"code_semantic", bundle.codeStore != nil,
	)

	return nil
}

// tracerFor returns the Langfuse tracer the next bundle should use.
//
// When the live bundle already holds a tracer built from an equivalent
// configuration it is handed over unchanged. That matters because the exporter
// owns a batch queue: tearing it down while a docs build is mid-flight drops
// whatever it had not yet shipped, and most settings changes (a model name, a
// chunk size) have nothing to do with observability. Only a genuine
// observability change pays for a rebuild.
//
// A tracer that fails to build is logged and replaced by an inert one:
// telemetry must never be the reason a settings save fails.
func (m *Manager) tracerFor(s settings.Settings) *langfuse.Tracer {
	cfg := langfuseConfig(s)

	m.docsMu.RLock()
	prev := m.docs
	m.docsMu.RUnlock()

	if prev != nil && prev.tracer.Same(cfg) {
		return prev.tracer
	}

	tracer, err := langfuse.New(cfg)
	if err != nil {
		slog.Error("langfuse export disabled", "error", err)

		return tracer // langfuse.New returns an inert tracer alongside its error
	}

	if tracer.Enabled() {
		slog.Info("langfuse export enabled",
			"host", cfg.Host, "environment", cfg.Environment, "capture", cfg.Capture)
	}

	return tracer
}

// Tracer returns the live Langfuse tracer. It is never nil, so callers outside
// the docs bundle (the MCP server, HTTP middleware) can instrument
// unconditionally.
//
// The nil-receiver case is real: the MCP server can be constructed without a
// manager (tool-catalog inspection), and its tracing middleware runs on every
// request regardless.
func (m *Manager) Tracer() *langfuse.Tracer {
	if m == nil {
		return langfuse.Disabled()
	}

	m.docsMu.RLock()
	defer m.docsMu.RUnlock()

	if m.docs == nil || m.docs.tracer == nil {
		return langfuse.Disabled()
	}

	return m.docs.tracer
}

// buildBundle constructs docgen/rag clients from settings. A disabled or
// unconfigured capability yields a nil field rather than an error, so partial
// configuration (e.g. docs on, rag off) is valid. Store construction failures
// leave the previous live bundle active.
func (m *Manager) buildBundle(s settings.Settings) (*docsBundle, error) {
	b := &docsBundle{ragCfg: ragConfig(s), imageCfg: webImageConfig(s), tracer: m.tracerFor(s)}

	var (
		codeEmb   *embedder.Client
		codeStore vectorstore.Store
	)

	// Doc generation needs a chat LLM.
	if s.DocsEnabled {
		chat, err := llm.New(llmConfig(s), llm.WithTracer(b.tracer))
		switch {
		case errors.Is(err, llm.ErrNotConfigured):
			slog.Warn("docs enabled but llm not configured; doc generation disabled")
		case err != nil:
			return nil, fmt.Errorf("build llm client; %w", err)
		default:
			// A dedicated (usually faster) model for the per-file summary phase;
			// falls back to the synthesis client when unset or misconfigured.
			summary := chat
			if sc, serr := llm.New(summaryLLMConfig(s), llm.WithTracer(b.tracer)); serr == nil {
				summary = sc
			}

			b.gen = docgen.New(docsConfig(s), chat, summary, m.engine, b.tracer)
		}
	}

	if s.WebImageAnalysisEnabled {
		vision, err := llm.New(visionLLMConfig(s), llm.WithTracer(b.tracer))
		switch {
		case errors.Is(err, llm.ErrNotConfigured):
			slog.Warn("web image analysis enabled but llm not configured; vision disabled")
		case err != nil:
			return nil, fmt.Errorf("build vision client; %w", err)
		default:
			b.vision = vision
		}
	}

	// RAG needs an embedder and a vector store.
	if s.RAGEnabled {
		emb, err := embedder.New(embedderConfig(s), embedder.WithTracer(b.tracer))
		switch {
		case errors.Is(err, embedder.ErrNotConfigured):
			slog.Warn("rag enabled but embedder not configured; rag disabled")
		case err != nil:
			return nil, fmt.Errorf("build embedder client; %w", err)
		default:
			store, serr := vectorstore.New(m.docsVectorsDir)
			if serr != nil {
				return nil, fmt.Errorf("build vector store; %w", serr)
			}

			b.store = store
			b.rag = rag.New(ragConfig(s), emb, store)

			logVectorCacheFit("docs", s.EmbedDim)
		}
	}

	// Code RAG has its own on/off switch and (optionally) its own embedder; it
	// indexes into a separate store namespace so docs/code dimensions never
	// collide.
	if s.CodeRAGEnabled {
		emb, err := embedder.New(codeEmbedderConfig(s), embedder.WithTracer(b.tracer))
		switch {
		case errors.Is(err, embedder.ErrNotConfigured):
			slog.Warn("code rag enabled but no embedder configured; code rag disabled")
		case err != nil:
			return nil, fmt.Errorf("build code embedder client; %w", err)
		default:
			store, serr := vectorstore.New(m.codeVectorsDir)
			if serr != nil {
				if b.store != nil {
					_ = b.store.Close()
				}

				return nil, fmt.Errorf("build code vector store; %w", serr)
			}

			codeEmb = emb
			codeStore = store

			logVectorCacheFit("code", s.CodeEmbedDim)
		}
	}

	b.codeStore = codeStore
	b.codeRag = coderag.New(codeRagConfig(s), codeEmb, codeStore, m.engine, m.codeText)

	return b, nil
}

// logVectorCacheFit reports how much of a vector index the decoded-embedding
// cache can hold at the configured width.
//
// The number is worth stating out loud because it is the one cache whose
// useful size is decided by the embedding model, not the machine: an entry
// costs dim*4 bytes, so tripling the model's width cuts the cache to a third
// of the vectors without any config having changed. Past that point a search
// pays a read and a decode on nearly every node it visits, and because
// eviction is random the degradation is abrupt rather than gradual.
//
// A width of zero means the provider's native dimension, which is not known
// until the first embedding call, so there is nothing useful to report.
func logVectorCacheFit(index string, dim int) {
	if dim <= 0 {
		return
	}

	budget := memlimit.Current()

	slog.Info("vector cache",
		"index", index,
		"dim", dim,
		"budget", memlimit.Bytes(budget.VectorCache),
		"holds_vectors", budget.VectorCacheFit(dim),
	)
}

// TestResult reports the outcome of a connectivity/credentials test.
type TestResult struct {
	OK        bool   `json:"ok"`
	Model     string `json:"model,omitempty"`
	Dim       int    `json:"dim,omitempty"` // embedder only
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// mergeSecrets fills blank secret fields in patch from the currently stored
// settings, so the UI can test un-saved changes without re-sending stored
// secrets (typed key wins; blank = use stored).
func (m *Manager) mergeSecrets(ctx context.Context, patch settings.Settings) (settings.Settings, error) {
	if m.settings == nil {
		return patch, nil
	}

	cur, err := m.settings.Get(ctx)
	if err != nil {
		return patch, err
	}

	if patch.LLMAPIKey == "" {
		patch.LLMAPIKey = cur.LLMAPIKey
	}

	if patch.EmbedAPIKey == "" {
		patch.EmbedAPIKey = cur.EmbedAPIKey
	}

	if patch.CodeEmbedAPIKey == "" {
		patch.CodeEmbedAPIKey = cur.CodeEmbedAPIKey
	}

	if patch.LangfuseSecretKey == "" {
		patch.LangfuseSecretKey = cur.LangfuseSecretKey
	}

	return patch, nil
}

// TestLangfuse validates the Langfuse host and project keys using the given
// (un-saved) settings. Blank secrets fall back to the stored value; nothing is
// persisted.
//
// It calls the public projects endpoint rather than the OTLP one: OTLP accepts
// a batch and answers 207 regardless of whether the credentials resolve to a
// project, so it cannot distinguish a working key from a typo. The projects
// endpoint authenticates with the same Basic credentials and answers 401 on a
// bad key, which is the question being asked.
func (m *Manager) TestLangfuse(ctx context.Context, patch settings.Settings) TestResult {
	s, err := m.mergeSecrets(ctx, patch)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	cfg := langfuseConfig(s)

	host := strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	if host == "" || cfg.PublicKey == "" || cfg.SecretKey == "" {
		return TestResult{Error: "langfuse host, public key and secret key are required"}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/public/projects", nil)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)

	start := time.Now()
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	res := TestResult{LatencyMS: time.Since(start).Milliseconds()}

	if err != nil {
		res.Error = err.Error()

		return res
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Error = fmt.Sprintf("langfuse http %d; %s", resp.StatusCode, strings.TrimSpace(string(body)))

		return res
	}

	// Report which project the keys resolve to: the most common misconfiguration
	// is a valid key pair pointed at the wrong project.
	//
	// A decode failure is fatal rather than cosmetic. Anything with a catch-all
	// route answers 200 to this path — krabby's own SPA fallback does — so
	// without checking the shape the test would pass against any web server
	// that happens to be running at the configured address.
	var projects struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}

	if jerr := json.Unmarshal(body, &projects); jerr != nil {
		res.Error = fmt.Sprintf("%s did not answer with a Langfuse project list; check the host", host)

		return res
	}

	if len(projects.Data) > 0 {
		res.Model = projects.Data[0].Name
	}

	// Valid keys do not imply a reachable OTLP endpoint. The two live on
	// different paths, and a Langfuse older than v3.22.0 serves the API while
	// answering 404 for OTLP — which would leave the test green while every
	// export was silently discarded.
	if err := probeOTLP(ctx, host, cfg, timeout); err != nil {
		res.Error = err.Error()

		return res
	}

	res.OK = true

	return res
}

// probeOTLP verifies that the traces endpoint exists and accepts the
// credentials.
//
// The body is empty on purpose: zero bytes is a valid, empty
// ExportTraceServiceRequest in protobuf, so the probe writes no trace and
// leaves no residue. Only the endpoint's existence is being asked about.
func probeOTLP(ctx context.Context, host string, cfg config.Langfuse, timeout time.Duration) error {
	endpoint := host + langfuse.TracesPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("otlp endpoint %s unreachable; %w", endpoint, err)
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("otlp endpoint %s returned 404; the Langfuse instance is older than v3.22.0 or the host is wrong", endpoint)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("otlp endpoint %s rejected the project keys (http %d)", endpoint, resp.StatusCode)
	case resp.StatusCode >= 500:
		return fmt.Errorf("otlp endpoint %s returned http %d", endpoint, resp.StatusCode)
	}

	// A 2xx or a 4xx about the payload both prove the endpoint is there and
	// the credentials pass, which is all this probe can establish.
	return nil
}

// TestLLM validates the chat LLM using the given (un-saved) settings. Blank
// secrets fall back to the stored value. It never persists anything.
func (m *Manager) TestLLM(ctx context.Context, patch settings.Settings) TestResult {
	s, err := m.mergeSecrets(ctx, patch)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	client, err := llm.New(llmConfig(s))
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	start := time.Now()
	err = client.Ping(ctx)
	res := TestResult{
		Model:     client.Model(),
		LatencyMS: time.Since(start).Milliseconds(),
	}

	if err != nil {
		res.Error = err.Error()

		return res
	}

	res.OK = true

	return res
}

// TestEmbedder validates the embeddings endpoint using the given (un-saved)
// settings. Blank secrets fall back to the stored value. It never persists.
func (m *Manager) TestEmbedder(ctx context.Context, patch settings.Settings) TestResult {
	s, err := m.mergeSecrets(ctx, patch)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	client, err := embedder.New(embedderConfig(s))
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	start := time.Now()
	err = client.Ping(ctx)
	res := TestResult{
		Model:     client.Model(),
		LatencyMS: time.Since(start).Milliseconds(),
	}

	if err != nil {
		res.Error = err.Error()

		return res
	}

	res.OK = true
	res.Dim = client.Dim()

	return res
}

// TestCodeEmbedder validates the code embeddings endpoint using the given
// (un-saved) settings. Blank secrets fall back to the stored value; a blank
// code embedder falls back to the docs embedder settings. It never persists.
func (m *Manager) TestCodeEmbedder(ctx context.Context, patch settings.Settings) TestResult {
	s, err := m.mergeSecrets(ctx, patch)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	client, err := embedder.New(codeEmbedderConfig(s))
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	start := time.Now()
	err = client.Ping(ctx)
	res := TestResult{
		Model:     client.Model(),
		LatencyMS: time.Since(start).Milliseconds(),
	}

	if err != nil {
		res.Error = err.Error()

		return res
	}

	res.OK = true
	res.Dim = client.Dim()

	return res
}

// ---- settings -> config adapters -------------------------------------------
// The client constructors take config.* structs; these translate the mutable
// settings record into them.

func docsConfig(s settings.Settings) config.Docs {
	return config.Docs{
		Enabled:      s.DocsEnabled,
		Concurrency:  s.DocsConcurrency,
		SummaryModel: s.DocsSummaryModel,
		MaxGroups:    s.DocsMaxGroups,
		Filters: config.Filters{
			Include:      s.DocsInclude,
			IncludeExtra: s.DocsIncludeExtra,
			Exclude:      s.DocsExclude,
		},
		Prompt:      s.DocsPrompt,
		PromptExtra: s.DocsPromptExtra,
		Limits: config.DocsLimits{
			MaxSourceBytes:    s.DocsMaxSourceBytes,
			MaxGroupBytes:     s.DocsMaxGroupBytes,
			MaxSynthesisBytes: s.DocsMaxSynthesisBytes,
		},
	}
}

func llmConfig(s settings.Settings) config.LLM {
	return config.LLM{
		BaseURL: s.LLMBaseURL,
		APIKey:  s.LLMAPIKey,
		Model:   s.LLMModel,
		Timeout: s.LLMTimeout,
	}
}

// summaryLLMConfig returns the LLM config for the per-file summary phase. It
// reuses the main chat endpoint/credentials/timeout and only overrides the model
// with the configured (usually faster) summary model. When no summary model is
// set it falls back to the main model, so the returned client behaves like the
// synthesis client.
func summaryLLMConfig(s settings.Settings) config.LLM {
	cfg := llmConfig(s)
	if m := strings.TrimSpace(s.DocsSummaryModel); m != "" {
		cfg.Model = m
	}

	return cfg
}

func visionLLMConfig(s settings.Settings) config.LLM {
	cfg := llmConfig(s)
	if model := strings.TrimSpace(s.WebImageModel); model != "" {
		cfg.Model = model
	}
	return cfg
}

func webImageConfig(s settings.Settings) config.WebImage {
	return config.WebImage{
		AnalysisEnabled:    s.WebImageAnalysisEnabled,
		Model:              visionLLMConfig(s).Model,
		MaxPerPage:         s.EffectiveWebImageMaxPerPage(),
		MaxBytes:           s.EffectiveWebImageMaxBytes(),
		MaxPixels:          s.EffectiveWebImageMaxPixels(),
		AllowAuthenticated: s.WebImageAllowAuthenticated,
	}
}

func embedderConfig(s settings.Settings) config.Embedder {
	return config.Embedder{
		BaseURL:     s.EmbedBaseURL,
		APIKey:      s.EmbedAPIKey,
		Model:       s.EmbedModel,
		Dim:         s.EmbedDim,
		Batch:       s.EmbedBatch,
		Concurrency: s.EmbedConcurrency,
		Timeout:     s.EmbedTimeout,
	}
}

// codeEmbedderConfig returns the code embedder settings, falling back to the
// docs embedder when no dedicated code embedder base URL is configured.
func codeEmbedderConfig(s settings.Settings) config.Embedder {
	if s.CodeEmbedBaseURL == "" {
		return embedderConfig(s)
	}

	return config.Embedder{
		BaseURL:     s.CodeEmbedBaseURL,
		APIKey:      s.CodeEmbedAPIKey,
		Model:       s.CodeEmbedModel,
		Dim:         s.CodeEmbedDim,
		Batch:       s.CodeEmbedBatch,
		Concurrency: s.CodeEmbedConcurrency,
		Timeout:     s.CodeEmbedTimeout,
	}
}

// langfuseConfig maps the persisted observability settings onto the exporter's
// configuration carrier.
func langfuseConfig(s settings.Settings) config.Langfuse {
	return config.Langfuse{
		Enabled:         s.LangfuseEnabled,
		Host:            s.LangfuseHost,
		PublicKey:       s.LangfusePublicKey,
		SecretKey:       s.LangfuseSecretKey,
		Environment:     s.LangfuseEnvironment,
		Timeout:         s.LangfuseTimeout,
		Capture:         config.ParseCapture(s.LangfuseCapture),
		MaxContentBytes: s.LangfuseMaxContentBytes,
		TraceDocs:       s.LangfuseTraceDocs,
		TraceEmbed:      s.LangfuseTraceEmbed,
		TraceMCP:        s.LangfuseTraceMCP,
		TraceHTTP:       s.LangfuseTraceHTTP,
	}
}

func ragConfig(s settings.Settings) config.RAG {
	return config.RAG{
		Enabled:             s.RAGEnabled,
		KeepMarkdownTargets: s.RAGKeepMarkdownTargets,
		ChunkSize:           s.RAGChunkSize,
		ChunkOverlap:        s.RAGChunkOverlap,
		TopK:                s.RAGTopK,
		TopDocs:             s.RAGTopDocs,

		HybridCandidates:     s.RAGHybridCandidates,
		HybridRRFK:           s.RAGHybridRRFK,
		HybridWeightLexical:  s.RAGHybridWeightLexical,
		HybridWeightSemantic: s.RAGHybridWeightSemantic,
		LexicalStopWords:     s.RAGLexicalStopWords,
	}
}

func codeRagConfig(s settings.Settings) config.CodeRAG {
	return config.CodeRAG{
		Enabled:      s.CodeRAGEnabled,
		ChunkSize:    s.CodeRAGChunkSize,
		ChunkOverlap: s.CodeRAGChunkOverlap,
		TopK:         s.CodeRAGTopK,
		Filters: config.Filters{
			Include:      s.CodeRAGInclude,
			IncludeExtra: s.CodeRAGIncludeExtra,
			Exclude:      s.CodeRAGExclude,
		},
	}
}

// ---- docs + RAG query surface ----------------------------------------------

// Docs search scopes: everything, repository docs, web sources or the API
// catalog.
const (
	ScopeAll     = "all"
	ScopeRepos   = "repos"
	ScopeSources = "sources"
	ScopeAPIs    = "apis"
)

// Documentation search modes. Semantic is the default when configured;
// otherwise lexical is used. Hybrid explicitly combines both ranks with RRF.
const (
	DocsSearchHybrid   = "hybrid"
	DocsSearchSemantic = "semantic"
	DocsSearchLexical  = "lexical"
)

// NormalizeDocsSearchMode validates a public docs search mode. An empty mode is
// left empty and resolved per request by resolveDocsSearchMode, because the
// default depends on what the installation has configured.
func NormalizeDocsSearchMode(mode string) (string, error) {
	switch mode = strings.ToLower(strings.TrimSpace(mode)); mode {
	case "", DocsSearchHybrid, DocsSearchSemantic, DocsSearchLexical:
		return mode, nil
	default:
		return "", fmt.Errorf("mode must be hybrid, semantic or lexical")
	}
}

// resolveDocsSearchMode picks the mode for a request that did not name one.
//
// Semantic is the default. The lexical arm's cost is proportional to how much
// of the corpus shares the question's vocabulary, and a large single-domain
// collection (a JIRA project of tens of thousands of tickets, say) is exactly
// the case where every term is common — so BM25 there scans most of the index
// while the vector search stays bounded by its ANN structure. Hybrid pays the
// lexical cost too, since it waits for both arms.
//
// Lexical remains the better tool for exact keys, error codes and identifiers,
// and hybrid for combining the two; both stay one parameter away. An
// installation with no embedder gets lexical, the only mode that works there.
func (m *Manager) resolveDocsSearchMode(mode string) string {
	if mode != "" {
		return mode
	}

	d, release := m.acquireDocs()
	defer release()

	if d.rag != nil {
		return DocsSearchSemantic
	}

	return DocsSearchLexical
}

// docsFilter translates a scope + optional key into a vector-store filter.
// key may be a repo id, a web-source scope key ("web:<name>") or an API-catalog
// scope key ("api:<name>"); when set it wins over the scope.
func docsFilter(scope, key string) (vectorstore.Filter, error) {
	if key != "" {
		return vectorstore.FilterKey(key), nil
	}

	switch scope {
	case "", ScopeAll:
		return vectorstore.Filter{}, nil
	case ScopeRepos:
		return vectorstore.Filter{Kind: vectorstore.KindRepo}, nil
	case ScopeSources:
		return vectorstore.Filter{Kind: vectorstore.KindWeb}, nil
	case ScopeAPIs:
		return vectorstore.Filter{Kind: vectorstore.KindAPI}, nil
	default:
		return vectorstore.Filter{}, fmt.Errorf("unknown scope %q (want all, repos, sources or apis)", scope)
	}
}

// WarmDocsSearch builds the persistent lexical index for existing markdown.
// It is local-only (no LLM or embedder calls) and safe to run in the background.
//
// Once it completes, every key that has markdown has a lexical index and the
// indexing pipeline keeps it that way, so queries stop checking (see
// SearchDocs).
func (m *Manager) WarmDocsSearch(ctx context.Context) error {
	if m.docsText == nil {
		return nil
	}

	if err := m.ensureDocsTextForSearch(ctx, ScopeAll, "", registry.NamespaceAll); err != nil {
		return err
	}

	if err := m.docsText.RefreshStats(ctx); err != nil {
		return err
	}

	m.docsTextWarmed.Store(true)

	return nil
}

// ensureDocsTextForSearch makes upgrade-safe lexical searches: installations
// with markdown created before the docs_search bucket existed are indexed on
// demand for exactly the keys participating in this query.
func (m *Manager) ensureDocsTextForSearch(ctx context.Context, scope, key, namespace string) error {
	if m.docsText == nil {
		return nil
	}

	if key != "" {
		if name := websource.CollectionName(key); name != "" {
			if m.sourcesRootDir == "" {
				return nil
			}

			return m.ensureDocsTextKey(ctx, key, m.sourcesDir(name))
		}

		repo, err := m.reg.Get(ctx, key)
		if err != nil || repo == nil {
			return err
		}
		docsDir, err := m.docsDirForRepo(repo)
		if err != nil {
			return err
		}

		return m.ensureDocsTextKey(ctx, repo.ID, docsDir)
	}

	var errs []error
	if scope == "" || scope == ScopeAll || scope == ScopeRepos {
		repos, err := m.reg.List(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, repo := range repos {
				if !strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll) && !repoInNamespace(repo, namespace) {
					continue
				}
				docsDir, derr := m.docsDirForRepo(repo)
				if derr == nil {
					derr = m.ensureDocsTextKey(ctx, repo.ID, docsDir)
				}
				if derr != nil {
					errs = append(errs, fmt.Errorf("warm docs text for %s; %w", repo.ID, derr))
				}
			}
		}
	}

	if (scope == "" || scope == ScopeAll || scope == ScopeSources) && m.webStore != nil && m.sourcesRootDir != "" {
		collections, err := m.webStore.ListCollections(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, collection := range collections {
				key := websource.ScopeKey(collection.Name)
				if err := m.ensureDocsTextKey(ctx, key, m.sourcesDir(collection.Name)); err != nil {
					errs = append(errs, fmt.Errorf("warm docs text for %s; %w", key, err))
				}
			}
		}
	}

	return errors.Join(errs...)
}

// ensureDocsTextKey backfills one key's lexical index, but never at the cost of
// the query it serves. The per-key lock is also held for the whole of a web
// source sync or a repo refresh/generate, so taking it unconditionally made a
// search block until an unrelated write job finished (and a scope-wide search
// blocks on every key it walks). Reads therefore probe first and only take the
// lock opportunistically: a key that is already indexed never touches it, and a
// key owned by a running job is skipped, because that job indexes it on
// completion anyway. Worst case the query runs against what is indexed now.
func (m *Manager) ensureDocsTextKey(ctx context.Context, key, docsDir string) error {
	// Keys already known to be indexed are never probed again: the pipeline
	// only ever adds to a key's index, so the answer cannot go back to "no".
	if _, ok := m.docsTextKeys.Load(key); ok {
		return nil
	}

	if !dirHasMarkdown(docsDir) {
		return nil
	}

	has, err := m.docsText.HasRepo(ctx, key)
	if err != nil {
		return err
	}
	if has {
		m.docsTextKeys.Store(key, struct{}{})

		return nil
	}

	// TryLock, not lockKey: a concurrent backfill of the same key is already
	// doing this work, so walking away is correct and waiting would only stall
	// a search behind it.
	lock := m.lockFor(key)
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()

	// Re-probe under the lock: a concurrent backfill may have finished while
	// this call was between the probe and the lock.
	if has, err = m.docsText.HasRepo(ctx, key); err != nil {
		return err
	}
	if !has {
		if err := m.docsText.IndexWithOptions(ctx, key, docsDir, &rag.IndexOptions{
			KeepMarkdownTargets: m.ragConfigSnapshot().KeepMarkdownTargets,
		}); err != nil {
			return err
		}
	}

	m.docsTextKeys.Store(key, struct{}{})

	return nil
}

// SearchDocs returns bounded markdown excerpts using hybrid, semantic or
// lexical retrieval. scope selects all/repos/sources; key restricts to one repo
// or web:<collection> and wins over scope. Namespace scopes only repository
// docs. topDocs <= 0 uses the default.
func (m *Manager) SearchDocs(ctx context.Context, scope, key, namespace, mode, question string, topDocs int) ([]rag.Doc, error) {
	searchStarted := time.Now()
	key = strings.TrimSpace(key)

	mode, err := NormalizeDocsSearchMode(mode)
	if err != nil {
		return nil, err
	}
	mode = m.resolveDocsSearchMode(mode)
	if err := m.validateDocsKey(ctx, key); err != nil {
		return nil, err
	}

	filter, err := docsFilter(scope, key)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(question) == "" {
		return nil, errors.New("question is empty")
	}
	if topDocs <= 0 {
		topDocs = rag.DefaultTopDocs
	}
	if topDocs > rag.MaxTopDocs {
		topDocs = rag.MaxTopDocs
	}

	filter, emptyScope, err := m.docsNamespaceFilter(ctx, scope, key, namespace, filter)
	if err != nil {
		return nil, err
	}
	if emptyScope {
		return []rag.Doc{}, nil
	}
	ragCfg := m.ragConfigSnapshot()

	// Both rankers are asked for the same candidate depth. An asymmetric depth
	// silently weights the longer list higher in rank fusion, because every
	// extra rank contributes more fused score.
	fetch := hybridCandidates(ragCfg)
	if fetch < topDocs {
		fetch = topDocs
	}

	if mode != DocsSearchSemantic {
		if m.docsText == nil {
			return nil, errors.New("lexical docs search is not configured")
		}
		// The lexical index is maintained by the indexing pipeline. The only
		// case a query could have to repair is an installation upgraded from
		// before the index existed, and the startup warm handles that once, so
		// this is skipped as soon as it has run. Probing per query and per key
		// in scope costs more than the search it guards.
		if !m.docsTextWarmed.Load() {
			if err := m.ensureDocsTextForSearch(ctx, scope, key, namespace); err != nil {
				return nil, err
			}
		}
	}

	// The two rankers read different stores — vectors from the embedded vector
	// database, BM25 from the state database — and share no mutable state, so
	// hybrid runs them together. Sequentially its latency was the sum of both
	// arms; now it is the slower one. The first failure cancels the other arm,
	// which matches the old behaviour of returning as soon as either failed.
	var (
		semanticDocs, lexicalDocs []rag.Doc
		semanticTook, lexicalTook time.Duration
		semanticSplit             rag.RetrieveTiming
	)

	group, groupCtx := errgroup.WithContext(ctx)
	if mode != DocsSearchLexical {
		group.Go(func() error {
			started := time.Now()
			docs, split, err := m.retrieveSemanticCandidates(groupCtx, filter, question, fetch)
			semanticDocs, semanticTook, semanticSplit = docs, time.Since(started), split

			return err
		})
	}

	if mode != DocsSearchSemantic {
		group.Go(func() error {
			started := time.Now()
			docs, err := m.searchLexical(groupCtx, filter, question, fetch, ragCfg)
			lexicalDocs, lexicalTook = docs, time.Since(started)

			return err
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	var docs []rag.Doc
	switch mode {
	case DocsSearchSemantic:
		docs = trimDocs(semanticDocs, topDocs)
	case DocsSearchLexical:
		docs = trimDocs(lexicalDocs, topDocs)
	default:
		docs = fuseDocs(lexicalDocs, semanticDocs, topDocs, fuseParamsFor(ragCfg))
	}

	m.enrichDocSources(ctx, docs)

	logDocsSearch(mode, scope, key, time.Since(searchStarted), semanticTook, lexicalTook, semanticSplit, len(docs))

	return docs, nil
}

func (m *Manager) validateDocsKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if strings.HasPrefix(key, websource.ScopePrefix) {
		name := websource.CollectionName(key)
		if name == "" || m.webStore == nil {
			return fmt.Errorf("unknown web source scope %q; use list_sources and pass its scope_key", key)
		}
		col, err := m.webStore.GetCollection(ctx, name)
		if err != nil {
			return err
		}
		if col == nil {
			return fmt.Errorf("unknown web source scope %q; use list_sources and pass its scope_key", key)
		}
		return nil
	}
	if m.reg == nil {
		return fmt.Errorf("unknown repository scope %q; use list_repos and pass its exact id", key)
	}
	repo, err := m.reg.Get(ctx, key)
	if err != nil {
		return err
	}
	if repo == nil {
		return fmt.Errorf("unknown repository scope %q; use list_repos and pass its exact id", key)
	}
	return nil
}

// docsNamespaceFilter resolves a repository namespace before retrieval. Web
// sources remain eligible for scope=all, but out-of-namespace repositories are
// never allowed to consume the bounded candidate window.
func (m *Manager) docsNamespaceFilter(ctx context.Context, scope, key, namespace string, filter vectorstore.Filter) (vectorstore.Filter, bool, error) {
	if key != "" || strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll) || scope == ScopeSources {
		return filter, false, nil
	}
	repos, err := m.reg.ListNamespace(ctx, namespace)
	if err != nil {
		return vectorstore.Filter{}, false, err
	}
	keys := make([]string, 0, len(repos))
	for _, repo := range repos {
		keys = append(keys, repo.ID)
	}
	if scope == "" || scope == ScopeAll {
		if m.webStore != nil {
			collections, err := m.webStore.ListCollections(ctx)
			if err != nil {
				return vectorstore.Filter{}, false, err
			}
			for _, col := range collections {
				keys = append(keys, websource.ScopeKey(col.Name))
			}
		}
	}
	if len(keys) == 0 {
		return vectorstore.Filter{}, true, nil
	}
	sort.Strings(keys)
	return vectorstore.Filter{Keys: keys}, false, nil
}

// docsSearchSlowThreshold is when one search is worth a log line of its own.
// Retrieval cost grows with how much of the corpus shares the question's
// vocabulary, which is a property of the data rather than of the query, so the
// only way to know whether it has become a problem for a given installation is
// to record it.
const docsSearchSlowThreshold = 500 * time.Millisecond

// logDocsSearch records what a search cost, per ranker. Debug for the normal
// case, a warning past the threshold, so a corpus that has outgrown the lexical
// index announces itself instead of being felt as "search is sluggish".
//
// The semantic arm is broken down further into the embedder round trip and the
// vector search. Those two degrade for unrelated reasons and are fixed in
// different places, so a bare semantic_ms leaves the first diagnostic question
// unanswered.
func logDocsSearch(
	mode, scope, key string,
	total, semantic, lexical time.Duration,
	split rag.RetrieveTiming,
	results int,
) {
	attrs := []any{
		"mode", mode,
		"total_ms", total.Milliseconds(),
		"results", results,
	}
	if key != "" {
		attrs = append(attrs, "key", key)
	} else if scope != "" {
		attrs = append(attrs, "scope", scope)
	}
	if semantic > 0 {
		attrs = append(attrs, "semantic_ms", semantic.Milliseconds())
	}
	if split.Embed > 0 {
		attrs = append(attrs, "embed_ms", split.Embed.Milliseconds())
	}
	if split.Vector > 0 {
		attrs = append(attrs, "vector_ms", split.Vector.Milliseconds())
	}
	if lexical > 0 {
		attrs = append(attrs, "lexical_ms", lexical.Milliseconds())
	}

	if total >= docsSearchSlowThreshold {
		slog.Warn("slow docs search", attrs...)

		return
	}

	slog.Debug("docs search", attrs...)
}

// logCodeSearch is logDocsSearch for the semantic code arm.
//
// Code search had no timing of its own, so the only way to find out whether a
// slow code query was the embedder or the index was to attach a profiler to a
// running server. Same threshold and same split as the docs arm, so the two
// are comparable in one log stream.
func logCodeSearch(repo, namespace string, total time.Duration, split coderag.RetrieveTiming, results int) {
	attrs := []any{
		"total_ms", total.Milliseconds(),
		"results", results,
	}
	if repo != "" {
		attrs = append(attrs, "repo", repo)
	} else if namespace != "" {
		attrs = append(attrs, "namespace", namespace)
	}
	if split.Embed > 0 {
		attrs = append(attrs, "embed_ms", split.Embed.Milliseconds())
	}
	if split.Vector > 0 {
		attrs = append(attrs, "vector_ms", split.Vector.Milliseconds())
	}

	if total >= docsSearchSlowThreshold {
		slog.Warn("slow code search", attrs...)

		return
	}

	slog.Debug("code search", attrs...)
}

// searchLexical runs the BM25 arm.
//
// bw ANDs a bare query's terms, so the raw question is first rewritten into an
// OR chain plus required identifiers. Terms the corpus itself shows to be
// ubiquitous are dropped from that chain: BM25 scores them near zero anyway,
// while matching them costs a scan proportional to the whole index.
//
// Filtering is an optimisation and is never allowed to cost a result. On a
// corpus whose vocabulary is narrow enough that the question's words are all
// common, the filtered query can come back empty; the unfiltered query is then
// retried, paying the slow path only when it actually buys something.
func (m *Manager) searchLexical(ctx context.Context, filter vectorstore.Filter, question string, fetch int, ragCfg config.RAG) ([]rag.Doc, error) {
	stop := m.docsText.FrequentTerms(ctx).Merge(rag.NewStopWords(ragCfg.LexicalStopWords))

	query := rag.LexicalQuery(question, stop)
	docs, err := m.docsText.Search(ctx, filter, query, fetch)
	if err != nil || len(docs) > 0 {
		return docs, err
	}

	if unfiltered := rag.LexicalQuery(question, nil); unfiltered != query {
		return m.docsText.Search(ctx, filter, unfiltered, fetch)
	}

	return docs, nil
}

// ragConfigSnapshot returns the live docs retrieval tuning. It is read from the
// bundle so a settings update swaps it atomically together with the clients.
func (m *Manager) ragConfigSnapshot() config.RAG {
	d, release := m.acquireDocs()
	defer release()

	return d.ragCfg
}

// retrieveSemanticCandidates runs the embedding arm under the bundle lease,
// reporting how the arm's time split between the embedder and the vector
// index so a slow search says which of the two was responsible.
func (m *Manager) retrieveSemanticCandidates(
	ctx context.Context,
	filter vectorstore.Filter,
	question string,
	candidates int,
) ([]rag.Doc, rag.RetrieveTiming, error) {
	d, release := m.acquireDocs()
	defer release()

	if d.rag == nil {
		return nil, rag.RetrieveTiming{}, fmt.Errorf("semantic docs search is not enabled; use mode %q", DocsSearchLexical)
	}

	return d.rag.RetrieveCandidatesTimed(ctx, filter, question, candidates)
}

func trimDocs(docs []rag.Doc, topDocs int) []rag.Doc {
	if len(docs) > topDocs {
		return docs[:topDocs]
	}

	return docs
}

// Hybrid rank-fusion defaults, applied when the corresponding setting is zero
// (including records written before the settings existed).
const (
	defaultHybridCandidates = 12
	// defaultHybridRRFK is deliberately far below the classic RRF constant of
	// 60. That value was tuned for thousand-deep TREC runs; over a list of
	// ~12 candidates it compresses rank 1 and rank 12 to within 18% of each
	// other, which makes "appears in both lists" outweigh rank quality
	// entirely.
	defaultHybridRRFK    = 20
	defaultHybridWeight  = 1.0
	maxHybridCandidates  = rag.MaxCandidates
	minHybridCandidates  = 1
	minHybridRRFKAllowed = 0
)

// fuseParams tunes reciprocal rank fusion. Zero fields take the defaults.
type fuseParams struct {
	K          int
	WLex, WSem float64
}

func fuseParamsFor(cfg config.RAG) fuseParams {
	return fuseParams{
		K:    cfg.HybridRRFK,
		WLex: cfg.HybridWeightLexical,
		WSem: cfg.HybridWeightSemantic,
	}
}

func (p fuseParams) normalized() fuseParams {
	if p.K <= minHybridRRFKAllowed {
		p.K = defaultHybridRRFK
	}
	if p.WLex <= 0 {
		p.WLex = defaultHybridWeight
	}
	if p.WSem <= 0 {
		p.WSem = defaultHybridWeight
	}

	return p
}

// hybridCandidates is how many documents each ranker contributes to fusion.
func hybridCandidates(cfg config.RAG) int {
	n := cfg.HybridCandidates
	if n <= 0 {
		n = defaultHybridCandidates
	}
	if n < minHybridCandidates {
		n = minHybridCandidates
	}
	if n > maxHybridCandidates {
		n = maxHybridCandidates
	}

	return n
}

// fuseDocs combines BM25 and semantic ranks with weighted reciprocal rank
// fusion, which avoids comparing the two rankers' unrelated raw score scales.
//
// Both lists must be fetched at the same depth: a ranker that contributes more
// ranks also contributes more total fused score, so an asymmetric depth is an
// implicit weight. Use p.WLex/p.WSem to weight a ranker on purpose instead.
//
// Ties are broken on repo+path so the result is independent of map iteration
// and of which list happened to be scanned first. The excerpt of the
// better-ranked occurrence is kept, so a document found by both rankers still
// shows the text that matched exactly.
func fuseDocs(lexical, semantic []rag.Doc, topDocs int, p fuseParams) []rag.Doc {
	p = p.normalized()

	type fusedDoc struct {
		key      string
		doc      rag.Doc
		score    float64
		bestRank int
	}

	byKey := map[string]*fusedDoc{}

	for _, ranking := range []struct {
		docs   []rag.Doc
		weight float64
	}{
		{lexical, p.WLex},
		{semantic, p.WSem},
	} {
		for i, doc := range ranking.docs {
			key := doc.Repo + "\x00" + doc.Path
			entry := byKey[key]
			if entry == nil {
				entry = &fusedDoc{key: key, doc: doc, bestRank: i}
				byKey[key] = entry
			} else if i < entry.bestRank {
				entry.doc = doc
				entry.bestRank = i
			}
			entry.score += ranking.weight / float64(p.K+i+1)
		}
	}

	fused := make([]*fusedDoc, 0, len(byKey))
	for _, entry := range byKey {
		fused = append(fused, entry)
	}
	sort.Slice(fused, func(i, j int) bool {
		if fused[i].score == fused[j].score {
			return fused[i].key < fused[j].key
		}

		return fused[i].score > fused[j].score
	})

	if len(fused) > topDocs {
		fused = fused[:topDocs]
	}
	docs := make([]rag.Doc, 0, len(fused))
	for _, entry := range fused {
		entry.doc.Score = float32(entry.score)
		docs = append(docs, entry.doc)
	}

	return docs
}

// filterDocsByNamespace keeps web-source docs (which are never namespaced) and
// repo docs whose repo is in the namespace, then trims to topDocs. It resolves
// the namespace's repo set once and matches doc.Repo against it.
func (m *Manager) filterDocsByNamespace(ctx context.Context, docs []rag.Doc, namespace string, topDocs int) ([]rag.Doc, error) {
	repos, err := m.reg.List(ctx)
	if err != nil {
		return nil, err
	}

	inNamespace := map[string]struct{}{}
	for _, repo := range repos {
		if repoInNamespace(repo, namespace) {
			inNamespace[repo.ID] = struct{}{}
		}
	}

	out := docs[:0]
	for _, doc := range docs {
		if strings.HasPrefix(doc.Repo, websource.ScopePrefix) {
			out = append(out, doc) // web source: always kept
			continue
		}
		if _, ok := inNamespace[doc.Repo]; ok {
			out = append(out, doc)
		}
	}

	if topDocs > 0 && len(out) > topDocs {
		out = out[:topDocs]
	}

	return out, nil
}

// enrichDocSources makes every broad-search result identify its exact scope and
// source kind. Web hits additionally carry collection metadata and item links.
func (m *Manager) enrichDocSources(ctx context.Context, docs []rag.Doc) {
	collections := map[string]*websource.Collection{}
	for i := range docs {
		docs[i].ScopeKey = docs[i].Repo
		name := websource.CollectionName(docs[i].Repo)
		if name == "" {
			docs[i].SourceKind = "repository"
			if m.reg != nil {
				repo, err := m.reg.Get(ctx, docs[i].Repo)
				if err == nil && repo != nil {
					docs[i].Namespace = registry.NormalizeNamespace(repo.Namespace)
					if docs[i].Namespace == "" {
						docs[i].Namespace = registry.NamespaceDefault
					}
				}
			}
			continue
		}
		docs[i].SourceKind = "web"
		docs[i].CollectionName = name
		if m.webStore == nil {
			continue
		}
		col, seen := collections[name]
		if !seen {
			col, _ = m.webStore.GetCollection(ctx, name)
			collections[name] = col
		}
		if col != nil {
			docs[i].CollectionType = col.Type
			docs[i].CollectionDescription = col.Description
		}

		slug := strings.TrimSuffix(docs[i].Path, ".md")
		page, err := m.webStore.GetPage(ctx, websource.PageID(name, slug))
		if err != nil || page == nil {
			continue
		}

		docs[i].URL = page.URL
		docs[i].Teams = page.Teams
	}
}

// namespaceScope describes how a namespace-restricted cross-repo search should
// run once the namespace is resolved to concrete repos.
//
//   - all: no restriction (the caller passed NamespaceAll or an explicit repo).
//   - single: exactly one repo in the namespace; search that repo directly.
//   - repos/set: several repos; search broadly and keep only these.
type namespaceScope struct {
	all    bool
	single string
	repos  []string
	set    map[string]struct{}
}

func (s namespaceScope) contains(repo string) bool {
	_, ok := s.set[repo]
	return ok
}

// fetch enlarges the requested topK for a post-filtered multi-repo search so the
// namespace filter does not starve the result set.
func (s namespaceScope) fetch(topK int) int {
	if topK <= 0 {
		topK = 10
	}
	scaled := topK * 4
	if scaled > 200 {
		scaled = 200
	}

	return scaled
}

// namespaceScope resolves a repoID/namespace pair. An explicit repoID or
// NamespaceAll yields an unrestricted (all) scope; otherwise it lists the repos
// in the namespace and reports whether it is empty, a single repo, or several.
func (m *Manager) namespaceScope(ctx context.Context, repoID, namespace string) (namespaceScope, error) {
	if repoID != "" || strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll) {
		return namespaceScope{all: true}, nil
	}

	repos, err := m.reg.List(ctx)
	if err != nil {
		return namespaceScope{}, err
	}

	scope := namespaceScope{set: map[string]struct{}{}}
	for _, repo := range repos {
		if repoInNamespace(repo, namespace) {
			scope.repos = append(scope.repos, repo.ID)
			scope.set[repo.ID] = struct{}{}
		}
	}
	sort.Strings(scope.repos)

	switch len(scope.repos) {
	case 0:
		return namespaceScope{}, fmt.Errorf("no repository in namespace %s; retry with namespace \"*\" to search all", displayNamespace(namespace))
	case 1:
		scope.single = scope.repos[0]
	}

	return scope, nil
}

func trimSnippets(snippets []coderag.Snippet, topK int) []coderag.Snippet {
	if topK <= 0 {
		topK = 10
	}
	if len(snippets) > topK {
		return snippets[:topK]
	}

	return snippets
}

// SearchCode returns the code snippets most relevant to a query. repoID == ""
// searches across the repos in namespace (empty or "default" == the default
// bucket; NamespaceAll searches every repo). topK <= 0 uses the code RAG
// default.
func (m *Manager) SearchCode(ctx context.Context, repoID, namespace, query string, topK int) ([]coderag.Snippet, error) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.codeRag == nil || d.codeStore == nil {
		return nil, ErrCodeRAGDisabled
	}

	scope, err := m.namespaceScope(ctx, repoID, namespace)
	if err != nil {
		return nil, err
	}

	searchStarted := time.Now()

	searchRepo := repoID
	fetch := scope.fetch(topK)
	if scope.single != "" {
		searchRepo, fetch = scope.single, topK
	}

	snippets, split, err := d.codeRag.RetrieveTimed(ctx, searchRepo, query, fetch)
	if err != nil {
		return nil, err
	}

	if scope.single == "" && !scope.all {
		out := snippets[:0]
		for _, s := range snippets {
			if scope.contains(s.Repo) {
				out = append(out, s)
			}
		}
		snippets = trimSnippets(out, topK)
	}

	logCodeSearch(repoID, namespace, time.Since(searchStarted), split, len(snippets))

	return snippets, nil
}

// SearchCodeText performs normal BM25 full-text search over the local bw index,
// scoped to namespace when repoID is empty.
func (m *Manager) SearchCodeText(
	ctx context.Context,
	repoID, namespace, query string,
	page, perPage int,
) (coderag.SearchPage, error) {
	if m.codeText == nil {
		return coderag.SearchPage{}, errors.New("normal code search is not configured")
	}

	scope, err := m.namespaceScope(ctx, repoID, namespace)
	if err != nil {
		return coderag.SearchPage{}, err
	}

	// The normal index may still be warming in the background at startup. Build
	// any in-scope repo's index on demand before searching so results are never
	// partial. Cheap when nothing is pending.
	if err := m.ensureCodeIndexForSearch(ctx, repoID, scope); err != nil {
		return coderag.SearchPage{}, err
	}

	if scope.single != "" {
		return m.codeText.Search(ctx, scope.single, query, page, perPage)
	}
	if scope.all {
		return m.codeText.Search(ctx, repoID, query, page, perPage)
	}

	// Namespace with several repos: BM25 search takes a single repo key, so fan
	// out over the namespace's repos and merge by score. Results are already
	// per-repo ranked; a stable score sort keeps the strongest hits on top.
	return m.searchCodeTextNamespace(ctx, scope, query, page, perPage)
}

// ensureCodeIndexForSearch warms the normal code index for every repo a
// SearchCodeText call will read, so a search issued while the background warm
// pass is still running blocks on the exact repos it needs instead of returning
// partial results. It resolves the in-scope repo ids from repoID/scope, skips
// any that are not pending, and builds the rest on demand (serialized per repo
// by ensureCodeIndex).
func (m *Manager) ensureCodeIndexForSearch(ctx context.Context, repoID string, scope namespaceScope) error {
	var ids []string
	switch {
	case scope.single != "":
		ids = []string{scope.single}
	case len(scope.repos) > 0:
		ids = scope.repos
	case scope.all && repoID != "":
		// Explicit single repo.
		ids = []string{repoID}
	case scope.all:
		// Cross-repo search ("*" or empty): every tracked repo participates.
		repos, err := m.reg.List(ctx)
		if err != nil {
			return err
		}
		ids = make([]string, 0, len(repos))
		for _, r := range repos {
			ids = append(ids, r.ID)
		}
	}

	var errs []error
	for _, id := range ids {
		if _, pending := m.codeWarmLock(id); !pending {
			continue
		}

		repo, err := m.reg.Get(ctx, id)
		if err != nil {
			// Repo vanished from the registry; drop it from pending and skip.
			m.clearCodeWarmPending(id)
			continue
		}
		if err := m.ensureCodeIndex(ctx, id, repo.Path); err != nil {
			errs = append(errs, fmt.Errorf("warm code index for %s: %w", id, err))
		}
	}

	return errors.Join(errs...)
}

func (m *Manager) searchCodeTextNamespace(
	ctx context.Context,
	scope namespaceScope,
	query string,
	page, perPage int,
) (coderag.SearchPage, error) {
	if perPage <= 0 {
		perPage = 10
	}
	if page <= 0 {
		page = 1
	}

	var all []coderag.Snippet
	for _, id := range scope.repos {
		// Pull enough from each repo to fill the requested page after merging.
		res, err := m.codeText.Search(ctx, id, query, 1, page*perPage)
		if err != nil {
			return coderag.SearchPage{}, err
		}
		all = append(all, res.Results...)
	}

	sort.SliceStable(all, func(i, j int) bool { return all[i].Score > all[j].Score })

	total := len(all)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}

	return coderag.SearchPage{
		Results: all[start:end],
		Total:   uint64(total),
		Page:    page,
		PerPage: perPage,
	}, nil
}

// ListDocs returns the generated doc metadata for a repo from its manifest.
func (m *Manager) ListDocs(ctx context.Context, repoID string) ([]docgen.DocMeta, error) {
	if m.docsRootDir == "" {
		return nil, ErrDocsDisabled
	}

	dir, err := m.repoDocsDir(ctx, repoID)
	if err != nil {
		return nil, err
	}

	man, err := docgen.LoadManifest(dir)
	if err != nil {
		return nil, err
	}

	if man == nil {
		return []docgen.DocMeta{}, nil
	}
	if len(man.Docs) == 0 {
		return []docgen.DocMeta{}, nil
	}

	return man.Docs, nil
}

// GetDoc returns one generated markdown doc. Path is relative to that repo's
// external docs directory and access is sandboxed to it.
func (m *Manager) GetDoc(ctx context.Context, repoID, docPath string, offset int64, maxBytes int) (*repofs.FileContent, error) {
	if m.docsRootDir == "" {
		return nil, ErrDocsDisabled
	}

	dir, err := m.repoDocsDir(ctx, repoID)
	if err != nil {
		return nil, err
	}

	return repofs.ReadFile(dir, docPath, offset, maxBytes)
}

// repoDocsDir resolves a docs key to its markdown directory: "web:<name>"
// keys map to the collection's synced content, repo ids to the repo's
// external docs directory (verifying the repo is tracked and cloned and
// migrating legacy in-clone docs when needed).
func (m *Manager) repoDocsDir(ctx context.Context, repoID string) (string, error) {
	if strings.HasPrefix(repoID, websource.ScopePrefix) {
		name := websource.CollectionName(repoID)
		if !websource.ValidName(name) {
			return "", fmt.Errorf("invalid web source scope %q", repoID)
		}
		if m.sourcesRootDir == "" {
			return "", ErrNoWebSources
		}
		if m.webStore == nil {
			return "", ErrNoWebSources
		}
		col, err := m.webStore.GetCollection(ctx, name)
		if err != nil {
			return "", err
		}
		if col == nil {
			return "", fmt.Errorf("web source %s not found", name)
		}

		return m.sourcesDir(name), nil
	}

	repo, err := m.reg.Get(ctx, repoID)
	if err != nil {
		return "", err
	}
	if repo == nil {
		return "", fmt.Errorf("repo %s not found", repoID)
	}
	if repo.Path == "" || !fileExists(filepath.Join(repo.Path, ".git")) {
		return "", fmt.Errorf("repo %s not cloned yet (status: %s)", repoID, repo.Status)
	}

	return m.docsDirForRepo(repo)
}
