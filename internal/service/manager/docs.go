package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/config"
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

	return m.setDocsConfig(ctx, patch)
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

	return m.setDocsConfig(ctx, next)
}

func (m *Manager) setDocsConfig(ctx context.Context, next settings.Settings) (settings.Redacted, error) {
	if m.settings == nil {
		return settings.Redacted{}, ErrNoSettingsStore
	}

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
	m.TriggerReindexAll()

	return redactSettings(saved), nil
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

	slog.Info("docs/rag reconfigured",
		"docgen", bundle.gen != nil,
		"rag", bundle.rag != nil,
		"code_semantic", bundle.codeStore != nil,
	)

	return nil
}

// buildBundle constructs docgen/rag clients from settings. A disabled or
// unconfigured capability yields a nil field rather than an error, so partial
// configuration (e.g. docs on, rag off) is valid. Store construction failures
// leave the previous live bundle active.
func (m *Manager) buildBundle(s settings.Settings) (*docsBundle, error) {
	b := &docsBundle{ragCfg: ragConfig(s)}
	var (
		codeEmb   *embedder.Client
		codeStore vectorstore.Store
	)

	// Doc generation needs a chat LLM.
	if s.DocsEnabled {
		chat, err := llm.New(llmConfig(s))
		switch {
		case errors.Is(err, llm.ErrNotConfigured):
			slog.Warn("docs enabled but llm not configured; doc generation disabled")
		case err != nil:
			return nil, fmt.Errorf("build llm client; %w", err)
		default:
			// A dedicated (usually faster) model for the per-file summary phase;
			// falls back to the synthesis client when unset or misconfigured.
			summary := chat
			if sc, serr := llm.New(summaryLLMConfig(s)); serr == nil {
				summary = sc
			}

			b.gen = docgen.New(docsConfig(s), chat, summary, m.engine)
		}
	}

	// RAG needs an embedder and a vector store.
	if s.RAGEnabled {
		emb, err := embedder.New(embedderConfig(s))
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
		}
	}

	// Code RAG has its own on/off switch and (optionally) its own embedder; it
	// indexes into a separate store namespace so docs/code dimensions never
	// collide.
	if s.CodeRAGEnabled {
		emb, err := embedder.New(codeEmbedderConfig(s))
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
		}
	}

	b.codeStore = codeStore
	b.codeRag = coderag.New(codeRagConfig(s), codeEmb, codeStore, m.engine, m.codeText)

	return b, nil
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

	return patch, nil
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
		Include:      s.DocsInclude,
		Exclude:      s.DocsExclude,
		Prompt:       s.DocsPrompt,
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

func ragConfig(s settings.Settings) config.RAG {
	return config.RAG{
		Enabled:      s.RAGEnabled,
		ChunkSize:    s.RAGChunkSize,
		ChunkOverlap: s.RAGChunkOverlap,
		TopK:         s.RAGTopK,
		TopDocs:      s.RAGTopDocs,

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
		Include:      s.CodeRAGInclude,
		Exclude:      s.CodeRAGExclude,
	}
}

// ---- docs + RAG query surface ----------------------------------------------

// Docs search scopes: everything, repository docs only, or web sources only.
const (
	ScopeAll     = "all"
	ScopeRepos   = "repos"
	ScopeSources = "sources"
)

// Documentation search modes. Hybrid is the default and combines semantic and
// lexical ranks with reciprocal rank fusion (RRF).
const (
	DocsSearchHybrid   = "hybrid"
	DocsSearchSemantic = "semantic"
	DocsSearchLexical  = "lexical"
)

// NormalizeDocsSearchMode validates a public docs search mode and applies the
// default used by REST, MCP and the UI.
func NormalizeDocsSearchMode(mode string) (string, error) {
	switch mode = strings.ToLower(strings.TrimSpace(mode)); mode {
	case "", DocsSearchHybrid:
		return DocsSearchHybrid, nil
	case DocsSearchSemantic, DocsSearchLexical:
		return mode, nil
	default:
		return "", fmt.Errorf("mode must be hybrid, semantic or lexical")
	}
}

// docsFilter translates a scope + optional key into a vector-store filter.
// key may be a repo id or a web-source scope key ("web:<name>"); when set it
// wins over the scope.
func docsFilter(scope, key string) (vectorstore.Filter, error) {
	if key != "" {
		return vectorstore.FilterKey(key), nil
	}

	switch scope {
	case "", ScopeAll:
		return vectorstore.Filter{}, nil
	case ScopeRepos:
		return vectorstore.Filter{ExcludePrefix: websource.ScopePrefix}, nil
	case ScopeSources:
		return vectorstore.Filter{Prefix: websource.ScopePrefix}, nil
	default:
		return vectorstore.Filter{}, fmt.Errorf("unknown scope %q (want all, repos or sources)", scope)
	}
}

// WarmDocsSearch builds the persistent lexical index for existing markdown.
// It is local-only (no LLM or embedder calls) and safe to run in the background.
func (m *Manager) WarmDocsSearch(ctx context.Context) error {
	if m.docsText == nil {
		return nil
	}

	if err := m.ensureDocsTextForSearch(ctx, ScopeAll, "", registry.NamespaceAll); err != nil {
		return err
	}

	return m.docsText.RefreshStats(ctx)
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
	if !dirHasMarkdown(docsDir) {
		return nil
	}

	has, err := m.docsText.HasRepo(ctx, key)
	if err != nil || has {
		return err
	}

	lock := m.lock(key)
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()

	// Re-probe under the lock: a concurrent backfill may have finished while
	// this call was between the probe and the lock.
	if has, err = m.docsText.HasRepo(ctx, key); err != nil || has {
		return err
	}

	return m.docsText.Index(ctx, key, docsDir)
}

// SearchDocs returns bounded markdown excerpts using hybrid, semantic or
// lexical retrieval. scope selects all/repos/sources; key restricts to one repo
// or web:<collection> and wins over scope. Namespace scopes only repository
// docs. topDocs <= 0 uses the default.
func (m *Manager) SearchDocs(ctx context.Context, scope, key, namespace, mode, question string, topDocs int) ([]rag.Doc, error) {
	mode, err := NormalizeDocsSearchMode(mode)
	if err != nil {
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

	nsFilter := key == "" && !strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll)
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
		if err := m.ensureDocsTextForSearch(ctx, scope, key, namespace); err != nil {
			return nil, err
		}
	}

	var semanticDocs, lexicalDocs []rag.Doc
	if mode != DocsSearchLexical {
		semanticDocs, err = m.retrieveSemanticCandidates(ctx, filter, question, fetch)
		if err != nil {
			return nil, err
		}
	}

	if mode != DocsSearchSemantic {
		lexicalDocs, err = m.searchLexical(ctx, filter, question, fetch, ragCfg)
		if err != nil {
			return nil, err
		}
	}

	if nsFilter {
		semanticDocs, err = m.filterDocsByNamespace(ctx, semanticDocs, namespace, 0)
		if err != nil {
			return nil, err
		}
		lexicalDocs, err = m.filterDocsByNamespace(ctx, lexicalDocs, namespace, 0)
		if err != nil {
			return nil, err
		}
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

	m.enrichWebDocs(ctx, docs)

	return docs, nil
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

// retrieveSemanticCandidates runs the embedding arm under the bundle lease.
func (m *Manager) retrieveSemanticCandidates(ctx context.Context, filter vectorstore.Filter, question string, candidates int) ([]rag.Doc, error) {
	d, release := m.acquireDocs()
	defer release()

	if d.rag == nil {
		return nil, fmt.Errorf("semantic docs search is not enabled; use mode %q", DocsSearchLexical)
	}

	return d.rag.RetrieveCandidates(ctx, filter, question, candidates)
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

// enrichWebDocs fills the original link and team names on web-source doc hits
// (e.g. JIRA tickets) from their persisted page records so search results can
// link back and show ownership. Missing records are left unenriched.
func (m *Manager) enrichWebDocs(ctx context.Context, docs []rag.Doc) {
	if m.webStore == nil {
		return
	}

	for i := range docs {
		name := websource.CollectionName(docs[i].Repo)
		if name == "" {
			continue
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
	if scope.single != "" {
		return d.codeRag.Retrieve(ctx, scope.single, query, topK)
	}

	snippets, err := d.codeRag.Retrieve(ctx, repoID, query, scope.fetch(topK))
	if err != nil {
		return nil, err
	}
	if scope.all {
		return snippets, nil
	}

	out := snippets[:0]
	for _, s := range snippets {
		if scope.contains(s.Repo) {
			out = append(out, s)
		}
	}

	return trimSnippets(out, topK), nil
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
	if name := websource.CollectionName(repoID); name != "" {
		if m.sourcesRootDir == "" {
			return "", ErrNoWebSources
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
