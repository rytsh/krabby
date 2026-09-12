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
	"github.com/rytsh/krabby/internal/service/apicatalog"
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

	var errs []error
	for _, repo := range repos {
		if err := m.TriggerRefresh(repo.ID); err != nil {
			errs = append(errs, fmt.Errorf("enqueue refresh for %s; %w", repo.ID, err))
		}
	}

	return errors.Join(errs...)
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
	var enqueueErrs []error
	if reindex {
		if err := m.TriggerReindexAll(); err != nil {
			enqueueErrs = append(enqueueErrs, fmt.Errorf("settings saved but reindex enqueue failed; %w", err))
		}
	}
	if imageChanged {
		if err := m.triggerImageSourceRefreshes(ctx); err != nil {
			enqueueErrs = append(enqueueErrs, fmt.Errorf("settings saved but image refresh enqueue failed; %w", err))
		}
	}
	if err := errors.Join(enqueueErrs...); err != nil {
		return redactSettings(saved), err
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

func (m *Manager) triggerImageSourceRefreshes(ctx context.Context) error {
	if m.webStore == nil {
		return nil
	}
	collections, err := m.webStore.ListCollections(ctx)
	if err != nil {
		return fmt.Errorf("list web sources for image refresh; %w", err)
	}
	var errs []error
	for _, col := range collections {
		if col.AnalyzeImages {
			if err := m.TriggerWebFullRefresh(col.Name); err != nil {
				errs = append(errs, fmt.Errorf("enqueue image refresh for %s; %w", col.Name, err))
			}
		}
	}

	return errors.Join(errs...)
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
// atomically. On success the previous bundle's resources are closed. On failure
// the previous (working) bundle is left in place and the error is returned so
// the caller (UI/MCP) can surface it.
//
// This is called once at startup with the persisted/seeded settings, and again
// on every settings update, giving live reconfiguration without a restart.
// tracerShutdownTimeout bounds the final flush of a replaced tracer. Spans
// that cannot be shipped in that window are dropped rather than delaying the
// reconfiguration.
const tracerShutdownTimeout = 5 * time.Second

// closeExcept releases every resource owned by b that is not handed to keep.
// A bundle rebuild may reuse the active tracer, so pointer identity is the
// ownership transfer: rollback keeps borrowed resources alive, while a
// successful swap leaves their eventual shutdown to the replacement bundle.
func (b *docsBundle) closeExcept(keep *docsBundle) error {
	if b == nil {
		return nil
	}

	var errs []error
	if b.tracer != nil && (keep == nil || b.tracer != keep.tracer) {
		ctx, cancel := context.WithTimeout(context.Background(), tracerShutdownTimeout)
		shutdown := b.tracerShutdown
		if shutdown == nil {
			shutdown = b.tracer.Shutdown
		}
		if err := shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown langfuse tracer; %w", err))
		}
		cancel()
	}

	keepsStore := func(store vectorstore.Store) bool {
		return keep != nil && (store == keep.store || store == keep.codeStore)
	}
	if b.store != nil && !keepsStore(b.store) {
		if err := b.store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close docs vector store; %w", err))
		}
	}
	if b.codeStore != nil && b.codeStore != b.store && !keepsStore(b.codeStore) {
		if err := b.codeStore.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close code vector store; %w", err))
		}
	}

	return errors.Join(errs...)
}

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

	if cerr := prev.closeExcept(bundle); cerr != nil {
		slog.Warn("close previous docs/rag bundle", "error", cerr)
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
func (m *Manager) tracerFor(s settings.Settings, prev *docsBundle) (*langfuse.Tracer, func(context.Context) error) {
	cfg := langfuseConfig(s)

	if prev != nil && prev.tracer.Same(cfg) {
		return prev.tracer, prev.tracerShutdown
	}

	newTracer := m.newLangfuseTracer
	if newTracer == nil {
		newTracer = langfuse.New
	}
	tracer, err := newTracer(cfg)
	if err != nil {
		slog.Error("langfuse export disabled", "error", err)
	}

	if tracer.Enabled() {
		slog.Info("langfuse export enabled",
			"host", cfg.Host, "environment", cfg.Environment, "capture", cfg.Capture)
	}

	shutdown := func(ctx context.Context) error {
		if m.shutdownLangfuseTracer != nil {
			return m.shutdownLangfuseTracer(ctx, tracer)
		}

		return tracer.Shutdown(ctx)
	}

	return tracer, shutdown
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
func (m *Manager) buildBundle(s settings.Settings) (_ *docsBundle, err error) {
	m.docsMu.RLock()
	prev := m.docs
	m.docsMu.RUnlock()

	tracer, shutdownTracer := m.tracerFor(s, prev)
	b := &docsBundle{
		ragCfg:         ragConfig(s),
		imageCfg:       webImageConfig(s),
		tracer:         tracer,
		tracerShutdown: shutdownTracer,
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if closeErr := b.closeExcept(prev); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback docs/rag bundle; %w", closeErr))
		}
	}()

	var codeEmb *embedder.Client
	openStore := m.openVectorStore
	if openStore == nil {
		openStore = vectorstore.New
	}

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
			store, serr := openStore(m.docsVectorsDir)
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
			store, serr := openStore(m.codeVectorsDir)
			if serr != nil {
				return nil, fmt.Errorf("build code vector store; %w", serr)
			}

			codeEmb = emb
			b.codeStore = store

			logVectorCacheFit("code", s.CodeEmbedDim)
		}
	}

	b.codeRag = coderag.New(codeRagConfig(s), codeEmb, b.codeStore, m.engine, m.codeText)

	committed = true
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
	defer func() { _ = resp.Body.Close() }()

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
	defer func() { _ = resp.Body.Close() }()

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

		if name := apicatalog.ServiceName(key); name != "" {
			if m.apisRootDir == "" {
				return nil
			}

			return m.ensureDocsTextKey(ctx, key, m.apisDir(name))
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

	if (scope == "" || scope == ScopeAll || scope == ScopeAPIs) && m.apiStore != nil && m.apisRootDir != "" {
		services, err := m.apiStore.ListServices(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, svc := range services {
				key := apicatalog.ScopeKey(svc.Name)
				if err := m.ensureDocsTextKey(ctx, key, m.apisDir(svc.Name)); err != nil {
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
	release, ok := m.tryLockKey(key)
	if !ok {
		return nil
	}
	defer release()

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
func (m *Manager) SearchDocs(ctx context.Context, scope, key, namespace, mode, question string, topDocs int) (rag.DocsPage, error) {
	searchStarted := time.Now()
	key = strings.TrimSpace(key)

	ctx, cancel := withSearchTimeout(ctx, semanticSearchTimeout)
	defer cancel()

	mode, err := NormalizeDocsSearchMode(mode)
	if err != nil {
		return rag.DocsPage{}, err
	}
	mode = m.resolveDocsSearchMode(mode)
	page := rag.DocsPage{Results: []rag.Doc{}, Mode: mode}

	if err := m.validateDocsKey(ctx, key); err != nil {
		return rag.DocsPage{}, err
	}

	filter, err := docsFilter(scope, key)
	if err != nil {
		return rag.DocsPage{}, err
	}
	if strings.TrimSpace(question) == "" {
		return rag.DocsPage{}, errors.New("question is empty")
	}

	ragCfg := m.ragConfigSnapshot()

	// The configured default is what an operator set through Settings; falling
	// straight to the package default would make that setting unreadable, so a
	// caller who raised it and then omitted top_docs would silently keep
	// getting three documents.
	if topDocs <= 0 {
		topDocs = ragCfg.TopDocs
	}
	if topDocs <= 0 {
		topDocs = rag.DefaultTopDocs
	}
	if topDocs > rag.MaxTopDocs {
		topDocs = rag.MaxTopDocs
	}

	filter, emptyScope, err := m.docsNamespaceFilter(ctx, scope, key, namespace, filter)
	if err != nil {
		return rag.DocsPage{}, err
	}
	if emptyScope {
		page.Note = fmt.Sprintf("namespace %s holds nothing indexed, so nothing was searched; retry with namespace \"*\" to search every namespace.",
			displayNamespace(namespace))

		return page, nil
	}

	// Both rankers are asked for the same candidate depth. An asymmetric depth
	// silently weights the longer list higher in rank fusion, because every
	// extra rank contributes more fused score.
	fetch := hybridCandidates(ragCfg)
	if fetch < topDocs {
		fetch = topDocs
	}

	if mode != DocsSearchSemantic {
		if m.docsText == nil {
			return rag.DocsPage{}, errors.New("lexical docs search is not configured")
		}
		// The lexical index is maintained by the indexing pipeline. The only
		// case a query could have to repair is an installation upgraded from
		// before the index existed, and the startup warm handles that once, so
		// this is skipped as soon as it has run. Probing per query and per key
		// in scope costs more than the search it guards.
		if !m.docsTextWarmed.Load() {
			if err := m.ensureDocsTextForSearch(ctx, scope, key, namespace); err != nil {
				return rag.DocsPage{}, err
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
		return rag.DocsPage{}, err
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

	if len(docs) > 0 {
		page.Results = docs

		return page, nil
	}

	page.Note = m.emptyDocsNote(mode, scope, key, namespace)

	return page, nil
}

// emptyDocsNote explains a docs search that matched nothing.
//
// Every branch below is a state an agent can act on, and none of them is
// distinguishable from "the documentation does not cover this" without being
// said out loud. The mode is named because it is not always the one asked for,
// and the scope because the default namespace is the easiest way to search a
// fraction of the corpus by accident.
func (m *Manager) emptyDocsNote(mode, scope, key, namespace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no document matched (mode %s", mode)
	switch {
	case key != "":
		fmt.Fprintf(&b, ", scope_key %s", key)
	case scope != "":
		fmt.Fprintf(&b, ", scope %s", scope)
	}
	if namespace != "" {
		fmt.Fprintf(&b, ", namespace %s", displayNamespace(namespace))
	}
	b.WriteString("). ")

	if mode == DocsSearchSemantic {
		b.WriteString("Semantic retrieval ranks the embedded documents, so a repository whose docs were generated but never embedded has nothing to rank: try mode 'lexical', or check the docs_index stage with repo_status. ")
	}
	if strings.HasPrefix(key, websource.ScopePrefix) || scope == ScopeSources {
		b.WriteString("This searched Krabby's indexed collection, not live Jira/Confluence. A missing hit does not prove the upstream item is absent: the collection may be filtered, incomplete or not synced. Use list_sources/get_source to inspect coverage, or the provider's live MCP when available.")
	} else {
		b.WriteString("Widen with namespace \"*\", or use list_docs to see whether the scope holds any document at all. Indexed source collections are not exhaustive live Jira/Confluence results.")
	}

	return b.String()
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
	if strings.HasPrefix(key, apicatalog.ScopePrefix) {
		name := apicatalog.ServiceName(key)
		if name == "" || m.apiStore == nil {
			return fmt.Errorf("unknown api scope %q; use list_api_services and pass api:<name>", key)
		}
		svc, err := m.apiStore.GetService(ctx, name)
		if err != nil {
			return err
		}
		if svc == nil {
			return fmt.Errorf("unknown api scope %q; use list_api_services and pass api:<name>", key)
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
// sources and catalogued APIs remain eligible for scope=all, but
// out-of-namespace repositories are never allowed to consume the bounded
// candidate window.
func (m *Manager) docsNamespaceFilter(ctx context.Context, scope, key, namespace string, filter vectorstore.Filter) (vectorstore.Filter, bool, error) {
	// Scopes that select no repository at all have nothing for a repository
	// namespace to narrow, so the filter passes through untouched. Building a
	// key allow-list for them would instead exclude everything they target.
	if key != "" || strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll) ||
		scope == ScopeSources || scope == ScopeAPIs {
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
		// Without this the allow-list silently excludes every catalogued API,
		// so a default scope=all search can never return an endpoint document.
		if m.apiStore != nil {
			services, err := m.apiStore.ListServices(ctx)
			if err != nil {
				return vectorstore.Filter{}, false, err
			}
			for _, svc := range services {
				keys = append(keys, apicatalog.ScopeKey(svc.Name))
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

// filterDocsByNamespace keeps web-source and API-catalog docs (neither is
// namespaced) and repo docs whose repo is in the namespace, then trims to
// topDocs. It resolves the namespace's repo set once and matches doc.Repo
// against it.
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
		// Web sources and catalogued APIs belong to no namespace, so a
		// namespace filter must pass them through rather than drop them for
		// failing a repo-set lookup they can never satisfy.
		if strings.HasPrefix(doc.Repo, websource.ScopePrefix) || strings.HasPrefix(doc.Repo, apicatalog.ScopePrefix) {
			out = append(out, doc)
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
// source kind. Web hits additionally carry collection metadata and item links,
// and API hits the service they belong to.
func (m *Manager) enrichDocSources(ctx context.Context, docs []rag.Doc) {
	collections := map[string]*websource.Collection{}
	services := map[string]*apicatalog.Service{}
	for i := range docs {
		docs[i].ScopeKey = docs[i].Repo

		if service := apicatalog.ServiceName(docs[i].Repo); service != "" {
			m.enrichAPIDoc(ctx, &docs[i], service, services)

			continue
		}

		name := websource.CollectionName(docs[i].Repo)
		if name == "" {
			docs[i].SourceKind = "repository"
			docs[i].Evidence = rag.DocEvidence{Kind: "generated_summary"}
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
		docs[i].Evidence = rag.DocEvidence{Kind: "synced_snapshot"}
		docs[i].CollectionName = name
		if m.webStore == nil {
			continue
		}
		col, seen := collections[name]
		if !seen {
			var err error
			// A lookup failure degrades this hit's metadata but must not fail
			// the search, so it is logged rather than returned. The result is
			// cached either way: one broken store should not be re-queried once
			// per hit. Bounded by distinct collection names per search.
			if col, err = m.webStore.GetCollection(ctx, name); err != nil {
				slog.Warn("enrich web doc: collection lookup failed",
					"collection", name, "error", err)
			}
			collections[name] = col
		}
		if col != nil {
			docs[i].CollectionType = col.Type
			docs[i].CollectionDescription = col.Description
			docs[i].Evidence.CollectionRefreshedAt = col.LastRefreshAt
			docs[i].Evidence.SyncStatus = col.Status
		}

		slug := docSlug(docs[i].Path)
		page, err := m.webStore.GetPage(ctx, websource.PageID(name, slug))
		if err != nil {
			// Debug rather than warn: this runs once per hit, so a store
			// outage would otherwise emit a line per result.
			slog.Debug("enrich web doc: page lookup failed",
				"collection", name, "slug", slug, "error", err)

			continue
		}
		if page == nil {
			continue
		}

		docs[i].URL = page.URL
		docs[i].Teams = page.Teams
		docs[i].Evidence.ItemStatus = page.Status
		docs[i].Evidence.IndexPending = page.IndexDirty
	}
}

// enrichAPIDoc annotates one API-catalog hit with the service it belongs to.
// The cache is per search, so a result set concentrated in one service reads
// the record once rather than per endpoint.
func (m *Manager) enrichAPIDoc(ctx context.Context, doc *rag.Doc, service string, cache map[string]*apicatalog.Service) {
	doc.SourceKind = "api"
	doc.Evidence = rag.DocEvidence{Kind: "catalog_snapshot"}
	doc.ServiceName = service

	if m.apiStore == nil {
		return
	}

	svc, seen := cache[service]
	if !seen {
		var err error
		// Logged, not returned: missing service metadata degrades the hit but
		// must not fail the search. Bounded by distinct service names.
		if svc, err = m.apiStore.GetService(ctx, service); err != nil {
			slog.Warn("enrich api doc: service lookup failed",
				"service", service, "error", err)
		}
		cache[service] = svc
	}
	if svc == nil {
		return
	}

	doc.ServiceGroup = svc.Group
	doc.ServiceBaseURL = svc.ResolvedBaseURL
	// The human override wins over the specification's own summary, exactly as
	// it does everywhere else the service is described.
	if doc.ServiceDescription = svc.Description; doc.ServiceDescription == "" {
		doc.ServiceDescription = svc.SpecSummary
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

// fetch enlarges the requested topK for a post-filtered search so the filters
// applied after retrieval do not starve the result set. filtered says whether
// anything will be dropped afterwards; without a post-filter the widening
// would only pay for candidates nobody looks at.
func (s namespaceScope) fetch(topK int, filtered bool) int {
	if topK <= 0 {
		topK = 10
	}
	if !filtered {
		return topK
	}

	// The ceiling matches coderag.maxSearchResults; asking for more would be
	// clamped one layer down and the widening would silently not happen.
	return min(topK*4, 200)
}

// namespaceScope resolves a repoID/namespace pair. An explicit repoID or
// NamespaceAll yields an unrestricted (all) scope; otherwise it lists the repos
// in the namespace and reports whether it is empty, a single repo, or several.
//
// A named repository is checked against the registry. Without that check a
// mistyped or untracked id scopes the search to a prefix nothing carries, and
// the caller is handed an empty page that reads as "this code does not exist"
// - the one answer a search must never invent. A namespace typo has always
// been an error; the id is the more likely typo of the two.
func (m *Manager) namespaceScope(ctx context.Context, repoID, namespace string) (namespaceScope, error) {
	if repoID = strings.TrimSpace(repoID); repoID != "" {
		repo, err := m.reg.Get(ctx, repoID)
		if err != nil {
			return namespaceScope{}, err
		}
		if repo == nil {
			return namespaceScope{}, fmt.Errorf("repository %q is not tracked; list_repos names the tracked ids, and omitting repo searches the default namespace", repoID)
		}

		return namespaceScope{all: true}, nil
	}

	if strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll) {
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
//
// Like the other two modes it honours a path glob and reports the index state
// of the repositories behind the page. Both matter more here than elsewhere: a
// vector index is built separately from the text one, so an empty semantic
// result on a repository whose embeddings were never built is otherwise
// indistinguishable from "this concept is not in the code".
func (m *Manager) SearchCode(
	ctx context.Context,
	repoID, namespace, query string,
	opts coderag.SemanticOptions,
) (coderag.SemanticPage, error) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.codeRag == nil || d.codeStore == nil {
		return coderag.SemanticPage{}, ErrCodeRAGDisabled
	}

	ctx, cancel := withSearchTimeout(ctx, semanticSearchTimeout)
	defer cancel()

	scope, err := m.namespaceScope(ctx, repoID, namespace)
	if err != nil {
		return coderag.SemanticPage{}, err
	}

	matchPath, err := coderag.Scope{Path: opts.Path}.PathMatcher()
	if err != nil {
		return coderag.SemanticPage{}, err
	}

	searchStarted := time.Now()

	byNamespace := scope.single == "" && !scope.all
	searchRepo := repoID
	if scope.single != "" {
		searchRepo = scope.single
	}
	fetch := scope.fetch(opts.TopK, byNamespace || matchPath != nil)

	snippets, split, err := d.codeRag.RetrieveTimed(ctx, searchRepo, query, fetch)
	if err != nil {
		return coderag.SemanticPage{}, err
	}

	if byNamespace || matchPath != nil {
		out := snippets[:0]
		for _, s := range snippets {
			if byNamespace && !scope.contains(s.Repo) {
				continue
			}
			if matchPath != nil && !matchPath(s.Path) {
				continue
			}
			out = append(out, s)
		}
		snippets = trimSnippets(out, opts.TopK)
	}

	logCodeSearch(repoID, namespace, time.Since(searchStarted), split, len(snippets))

	page := coderag.SemanticPage{Results: snippets}
	page.Indexed = m.repoIndexStates(ctx, m.indexedRepos(snippetRepos(snippets), m.scopedRepoIDs(ctx, repoID, scope)))

	return page, nil
}

// scopedRepoIDs lists the repositories a scope covers, for reporting the index
// state behind an empty result. A failure here costs the report, never the
// search: the hits are already resolved by the time it runs.
func (m *Manager) scopedRepoIDs(ctx context.Context, repoID string, scope namespaceScope) []string {
	ids, err := m.codeScopeRepos(ctx, repoID, scope)
	if err != nil {
		return nil
	}

	return ids
}

// SearchCodeText performs BM25 full-text search over the local bw index,
// scoped to a repository, a namespace or everything, and optionally to a path
// glob.
//
// bw ANDs a bare query's terms, so the raw query is first rewritten into an OR
// chain plus required identifiers — the same rewrite the documentation index
// uses, and for the same reason: passing a question verbatim requires every
// one of its words inside a single 3000-character chunk, which almost always
// matched nothing. Identifier-shaped words (`bw.ErrNotFound`, `PAY-1842`) stay
// required, so an exact lookup does not lose precision to the loosening.
//
// Terms the indexed source itself shows to be ubiquitous are dropped from that
// chain: a source corpus is full of `func`, `return` and `error`, each costing
// a posting-list walk proportional to the whole index while contributing
// almost nothing to the ranking. Filtering is an optimisation and is never
// allowed to cost a result, so a filtered query that comes back empty is
// retried unfiltered.
func (m *Manager) SearchCodeText(
	ctx context.Context,
	repoID, namespace, query string,
	opts coderag.TextSearchOptions,
) (page coderag.SearchPage, err error) {
	timing := newCodeSearchTiming()
	defer func() {
		timing.finish(ctx, slog.Default(), "text", repoID, namespace, len(page.Results), page.Total, err)
	}()
	if m.codeText == nil {
		return coderag.SearchPage{}, errors.New("normal code search is not configured")
	}

	ctx, cancel := withSearchTimeout(ctx, searchTimeout)
	defer cancel()

	filter, repos, err := m.codeScope(ctx, repoID, namespace, opts.Path, timing)
	if err != nil {
		return coderag.SearchPage{}, err
	}
	opts.KeyFilter = filter

	timing.enter("prepare")
	stop := m.codeText.FrequentTerms(ctx)
	rewritten := rag.LexicalQuery(query, stop)

	timing.enter("index")
	page, err = m.codeText.Search(ctx, rewritten, opts)
	if err != nil {
		return page, err
	}
	if page.Total == 0 {
		if unfiltered := rag.LexicalQuery(query, nil); unfiltered != rewritten {
			timing.enter("retry")
			timing.retried = true
			// The retry's failure must not be reported as an empty result: the
			// docstring promises filtering never costs a result, and a store
			// error presented as total=0 reads as a definitive "no such code".
			retry, rerr := m.codeText.Search(ctx, unfiltered, opts)
			if rerr != nil {
				return page, rerr
			}
			page = retry
		}
	}

	timing.enter("metadata")
	page.Indexed = m.repoIndexStates(ctx, m.indexedRepos(snippetRepos(page.Results), repos))

	return page, nil
}

// SearchCodeRegex runs a regular-expression search over the local trigram
// index, scoped the same way.
func (m *Manager) SearchCodeRegex(
	ctx context.Context,
	repoID, namespace, pattern string,
	opts coderag.RegexOptions,
) (page coderag.RegexPage, err error) {
	timing := newCodeSearchTiming()
	defer func() {
		timing.finish(ctx, slog.Default(), "regex", repoID, namespace, len(page.Results), page.Total, err)
	}()
	if m.codeText == nil {
		return coderag.RegexPage{}, errors.New("regex code search is not configured")
	}

	ctx, cancel := withSearchTimeout(ctx, searchTimeout)
	defer cancel()

	filter, repos, err := m.codeScope(ctx, repoID, namespace, opts.Path, timing)
	if err != nil {
		return coderag.RegexPage{}, err
	}

	timing.enter("index")
	page, err = m.codeText.SearchRegex(ctx, filter, pattern, opts)
	if err != nil {
		return page, err
	}

	timing.enter("metadata")
	hitRepos := make([]string, 0, len(page.Results))
	for _, hit := range page.Results {
		hitRepos = append(hitRepos, hit.Repo)
	}
	page.Indexed = m.repoIndexStates(ctx, m.indexedRepos(hitRepos, repos))

	return page, nil
}

// codeScope resolves a request's repo/namespace/path arguments into the chunk
// key filter every search mode shares, plus the concrete repository ids the
// search was scoped to.
//
// It also warms the index when the request named a single repository: that
// repo's index may still be building in the background at startup, and a query
// about one repo answered from a half-built index is a wrong answer. A
// cross-repo scope is not warmed here — see ensureCodeIndexForSearch.
func (m *Manager) codeScope(
	ctx context.Context,
	repoID, namespace, pathGlob string,
	timing *codeSearchTiming,
) (func(string) bool, []string, error) {
	timing.enter("scope")
	nsScope, err := m.namespaceScope(ctx, repoID, namespace)
	if err != nil {
		return nil, nil, err
	}

	timing.enter("warm")
	if err := m.ensureCodeIndexForSearch(ctx, repoID, nsScope); err != nil {
		return nil, nil, err
	}

	timing.enter("scope")
	repos, err := m.codeScopeRepos(ctx, repoID, nsScope)
	if err != nil {
		return nil, nil, err
	}

	// An unconstrained scope passes no repository list: with every tracked
	// repository in scope the prefix test can only ever succeed, so paying it
	// per hit buys nothing. An anchored path pattern is the exception — it
	// needs the list to know where the repository prefix ends.
	scope := coderag.Scope{Repos: repos, Path: pathGlob}
	if nsScope.all && repoID == "" && !strings.Contains(pathGlob, "/") {
		scope.Repos = nil
	}

	filter, err := scope.KeyFilter()
	if err != nil {
		return nil, nil, err
	}

	return filter, repos, nil
}

// codeScopeRepos lists the repository ids a scope covers.
func (m *Manager) codeScopeRepos(ctx context.Context, repoID string, scope namespaceScope) ([]string, error) {
	switch {
	case scope.single != "":
		return []string{scope.single}, nil
	case len(scope.repos) > 0:
		return scope.repos, nil
	case repoID != "":
		return []string{repoID}, nil
	}

	repos, err := m.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.ID)
	}

	return ids, nil
}

// repoIndexStatesLimit bounds how many repositories a result page reports the
// index state of beyond the ones that produced hits. A page carries at most
// PerPage distinct repositories, so the cap applies to the two cases that are
// scope-sized rather than page-sized: an empty result, and the list of repos
// still building. In both, "which repositories did this even look at" stops
// being useful long before it stops being long.
const repoIndexStatesLimit = 10

// repoIndexStates reports each repository's code-index state.
func (m *Manager) repoIndexStates(ctx context.Context, repos []string) []coderag.RepoIndex {
	if len(repos) == 0 {
		return nil
	}

	out := make([]coderag.RepoIndex, 0, len(repos))
	for _, id := range repos {
		repo, err := m.reg.Get(ctx, id)
		if err != nil || repo == nil {
			// A repository removed between the search and this read simply has
			// no index state to report; the hits it produced stay valid.
			continue
		}
		stage := repo.Stages.CodeIndex
		_, pending := m.codeWarmLock(id)
		out = append(out, coderag.RepoIndex{
			Repo:      id,
			Commit:    stage.Commit,
			IndexedAt: stage.FinishedAt,
			// The clone having moved past the indexed commit is the one thing
			// a caller cannot infer from the hit itself.
			Stale: stage.Commit != "" && repo.LastCommit != "" && stage.Commit != repo.LastCommit,
			// A cross-repo search no longer blocks on a first build, so the
			// page has to say which repos it could not read yet.
			Pending: pending,
		})
	}

	return out
}

// indexedRepos picks the repositories whose index state a page should report.
//
// Always the ones that produced hits. Plus any repository in scope whose first
// index is still being built: a cross-repo search does not wait for those, so
// leaving them out would present "not searched yet" as "nothing matched here",
// which is the one thing the caller cannot recover from the results. When
// nothing matched at all the scope itself is reported, for the same reason.
func (m *Manager) indexedRepos(hits, scoped []string) []string {
	seen := make(map[string]struct{}, len(hits))
	out := make([]string, 0, len(hits))

	add := func(id string) bool {
		if id == "" {
			return false
		}
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)

		return true
	}

	if len(hits) == 0 {
		for _, id := range scoped {
			if len(out) >= repoIndexStatesLimit {
				break
			}
			add(id)
		}

		return out
	}

	for _, id := range hits {
		add(id)
	}

	// The cap counts only the repositories added for being unbuilt; the hits
	// are already bounded by PerPage.
	unbuilt := 0
	for _, id := range scoped {
		if unbuilt >= repoIndexStatesLimit {
			break
		}
		if _, building := m.codeWarmLock(id); building && add(id) {
			unbuilt++
		}
	}

	return out
}

func snippetRepos(snippets []coderag.Snippet) []string {
	out := make([]string, 0, len(snippets))
	for _, s := range snippets {
		out = append(out, s.Repo)
	}

	return out
}

// ensureCodeIndexForSearch warms the normal code index on demand, but only for
// a search that named one repository.
//
// Waiting is the right answer for a single repo: the question is about that
// repo, so an index that is still building makes a partial result a wrong
// answer rather than an incomplete one, and the wait is bounded by one clone.
//
// A scope spanning many repositories is the opposite. Warming every one of them
// here puts a full chunk-and-index pass per repo inside the request, in series,
// behind the same per-repo permits the background warm holds — so the first
// cross-repo query after a bulk import waits for the entire corpus to be built
// and reads as a hang. Nothing about that wait is load-bearing: those repos are
// already being warmed in the background, and a page reports what is still
// pending (repoIndexStates), so the caller can tell "no matches" apart from
// "not indexed yet" without the request paying for the difference.
//
// This mirrors ensureDocsTextKey, which resolved the same problem on the
// lexical docs path.
func (m *Manager) ensureCodeIndexForSearch(ctx context.Context, repoID string, scope namespaceScope) error {
	target := scope.single
	if target == "" && scope.all && repoID != "" {
		// Explicit single repo.
		target = repoID
	}
	if target == "" {
		// Cross-repo search: report, do not block. See above.
		return nil
	}

	if _, pending := m.codeWarmLock(target); !pending {
		return nil
	}

	repo, err := m.reg.Get(ctx, target)
	if err != nil {
		return fmt.Errorf("load repo %s for code index warmup: %w", target, err)
	}
	if repo == nil {
		// A repository can be removed after namespace resolution or the
		// startup list. It no longer participates in this search.
		m.clearCodeWarmPending(target)

		return nil
	}
	if err := m.ensureCodeIndex(ctx, target, repo.Path); err != nil {
		return fmt.Errorf("warm code index for %s: %w", target, err)
	}

	return nil
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

	// No manifest means documentation generation has never completed for this
	// repository, which is a different answer from "this repository has no
	// documents" and one the caller can act on — the stage may be pending,
	// skipped through skip_stages, or have failed.
	if man == nil {
		return nil, fmt.Errorf("no generated documentation for %s yet; check repo_status for the docs stage, or refresh_repo to build it", repoID)
	}

	if len(man.Docs) == 0 {
		return []docgen.DocMeta{}, nil
	}

	return man.Docs, nil
}

// GetDoc returns one generated markdown doc. Path is relative to that repo's
// external docs directory and access is sandboxed to it.
func (m *Manager) GetDoc(ctx context.Context, repoID, docPath string, offset int64, maxBytes int) (*repofs.FileContent, error) {
	if m.docsRootDir == "" && !strings.HasPrefix(repoID, websource.ScopePrefix) && !strings.HasPrefix(repoID, apicatalog.ScopePrefix) {
		return nil, ErrDocsDisabled
	}

	dir, err := m.repoDocsDir(ctx, repoID)
	if err != nil {
		return nil, err
	}

	return repofs.ReadFile(dir, docPath, offset, maxBytes)
}

// repoDocsDir resolves a docs key to its markdown directory: "web:<name>"
// keys map to the collection's synced content, "api:<name>" keys to the
// service's endpoint projections, repo ids to the repo's external docs
// directory (verifying the repo is tracked and cloned and migrating legacy
// in-clone docs when needed).
func (m *Manager) repoDocsDir(ctx context.Context, repoID string) (string, error) {
	if strings.HasPrefix(repoID, apicatalog.ScopePrefix) {
		name := apicatalog.ServiceName(repoID)
		if !apicatalog.ValidName(name) {
			return "", fmt.Errorf("invalid api scope %q", repoID)
		}
		if m.apisRootDir == "" || m.apiStore == nil {
			return "", ErrNoAPICatalog
		}
		svc, err := m.apiStore.GetService(ctx, name)
		if err != nil {
			return "", err
		}
		if svc == nil {
			return "", fmt.Errorf("api service %s not found", name)
		}

		return m.apisDir(name), nil
	}

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
