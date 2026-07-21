package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// ErrDocsDisabled is returned by doc/RAG methods when the subsystem is off.
var ErrDocsDisabled = errors.New("docs/rag subsystem is not enabled")

// ErrNoSettingsStore is returned when config methods are called before a
// settings store has been attached.
var ErrNoSettingsStore = errors.New("settings store not configured")

// SetSettingsStore attaches the persisted settings store. Called once at
// startup, before Configure.
func (m *Manager) SetSettingsStore(s *settings.Store) {
	m.settings = s
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

	return s.Redact(), nil
}

// SetDocsConfig persists a settings patch (empty secrets keep existing values),
// then rebuilds the docs/RAG clients live. If the rebuild fails the previous
// working bundle stays active and the error is returned; the settings are still
// persisted so the user can correct them.
func (m *Manager) SetDocsConfig(ctx context.Context, patch settings.Settings) (settings.Redacted, error) {
	if m.settings == nil {
		return settings.Redacted{}, ErrNoSettingsStore
	}

	saved, err := m.settings.Set(ctx, patch)
	if err != nil {
		return settings.Redacted{}, err
	}

	if err := m.Configure(ctx, saved); err != nil {
		return saved.Redact(), fmt.Errorf("settings saved but rebuild failed; %w", err)
	}

	return saved.Redact(), nil
}

// Configure builds a new docs/RAG client bundle from s and swaps it in
// atomically. On success the previous bundle's store is closed. On failure the
// previous (working) bundle is left in place and the error is returned so the
// caller (UI/MCP) can surface it.
//
// This is called once at startup with the persisted/seeded settings, and again
// on every settings update, giving live reconfiguration without a restart.
func (m *Manager) Configure(_ context.Context, s settings.Settings) error {
	bundle, err := m.buildBundle(s)
	if err != nil {
		return err
	}

	m.docsMu.Lock()
	prev := m.docs
	m.docs = bundle
	m.docsMu.Unlock()

	// Close the store owned by the replaced bundle (if any and distinct).
	if prev != nil && prev.store != nil && prev.store != bundle.store {
		if cerr := prev.store.Close(); cerr != nil {
			slog.Warn("close previous vector store", "error", cerr)
		}
	}

	slog.Info("docs/rag reconfigured",
		"docgen", bundle.gen != nil,
		"rag", bundle.rag != nil,
	)

	return nil
}

// buildBundle constructs docgen/rag clients from settings. A disabled or
// unconfigured capability yields a nil field rather than an error, so partial
// configuration (e.g. docs on, rag off) is valid. Only genuine construction
// failures (bad store kind, unreachable qdrant setup) return an error.
func (m *Manager) buildBundle(s settings.Settings) (*docsBundle, error) {
	b := &docsBundle{}

	// Doc generation needs a chat LLM.
	if s.DocsEnabled {
		chat, err := llm.New(llmConfig(s))
		switch {
		case errors.Is(err, llm.ErrNotConfigured):
			slog.Warn("docs enabled but llm not configured; doc generation disabled")
		case err != nil:
			return nil, fmt.Errorf("build llm client; %w", err)
		default:
			b.gen = docgen.New(docsConfig(s), chat)
		}
	}

	// RAG needs an embedder and a vector store.
	if s.RAGEnabled {
		emb, err := embedder.New(embedderConfig(s))
		switch {
		case errors.Is(err, embedder.ErrNotConfigured):
			slog.Warn("rag enabled but embedder not configured; rag disabled")

			return b, nil
		case err != nil:
			return nil, fmt.Errorf("build embedder client; %w", err)
		}

		store, err := vectorstore.New(ragConfig(s), m.vectorsDir, emb.Dim())
		if err != nil {
			return nil, fmt.Errorf("build vector store; %w", err)
		}

		b.store = store
		b.rag = rag.New(ragConfig(s), emb, store)
	}

	return b, nil
}

// TestResult reports the outcome of a connectivity/credentials test.
type TestResult struct {
	OK         bool   `json:"ok"`
	Model      string `json:"model,omitempty"`
	Dim        int    `json:"dim,omitempty"`         // embedder only
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
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

	if patch.QdrantAPIKey == "" {
		patch.QdrantAPIKey = cur.QdrantAPIKey
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

// ---- settings -> config adapters -------------------------------------------
// The client constructors take config.* structs; these translate the mutable
// settings record into them.

func docsConfig(s settings.Settings) config.Docs {
	return config.Docs{
		Enabled:     s.DocsEnabled,
		Concurrency: s.DocsConcurrency,
		Include:     s.DocsInclude,
		Exclude:     s.DocsExclude,
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

func embedderConfig(s settings.Settings) config.Embedder {
	return config.Embedder{
		BaseURL: s.EmbedBaseURL,
		APIKey:  s.EmbedAPIKey,
		Model:   s.EmbedModel,
		Dim:     s.EmbedDim,
		Batch:   s.EmbedBatch,
		Timeout: s.EmbedTimeout,
	}
}

func ragConfig(s settings.Settings) config.RAG {
	return config.RAG{
		Enabled:      s.RAGEnabled,
		ChunkSize:    s.RAGChunkSize,
		ChunkOverlap: s.RAGChunkOverlap,
		TopK:         s.RAGTopK,
		TopDocs:      s.RAGTopDocs,
		Store: config.VectorStore{
			Kind: s.StoreKind,
			Qdrant: config.Qdrant{
				URL:        s.QdrantURL,
				APIKey:     s.QdrantAPIKey,
				Collection: s.QdrantCollection,
			},
		},
	}
}

// ---- docs + RAG query surface ----------------------------------------------

// SearchDocs returns the whole markdown documents most relevant to a question.
// repoID == "" searches across all repos. topDocs <= 0 uses the RAG default.
func (m *Manager) SearchDocs(ctx context.Context, repoID, question string, topDocs int) ([]rag.Doc, error) {
	d := m.currentDocs()
	if d.rag == nil {
		return nil, ErrDocsDisabled
	}

	return d.rag.Retrieve(ctx, repoID, question, topDocs)
}

// ListDocs returns the generated doc metadata for a repo from its manifest.
func (m *Manager) ListDocs(ctx context.Context, repoID string) ([]docgen.DocMeta, error) {
	if m.docsDir == nil {
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

	return man.Docs, nil
}

// GetDoc returns the contents of one generated markdown doc (path is relative to
// the repo's krabby-docs/ dir). Access is sandboxed to the docs dir.
func (m *Manager) GetDoc(ctx context.Context, repoID, docPath string) (*repofs.FileContent, error) {
	if m.docsDir == nil {
		return nil, ErrDocsDisabled
	}

	dir, err := m.repoDocsDir(ctx, repoID)
	if err != nil {
		return nil, err
	}

	return repofs.ReadFile(dir, docPath, 0, 0)
}

// repoDocsDir resolves a repo id to its docs directory, verifying the repo is
// tracked and cloned.
func (m *Manager) repoDocsDir(ctx context.Context, repoID string) (string, error) {
	dir, err := m.repoCloneDir(ctx, repoID)
	if err != nil {
		return "", err
	}

	return m.docsDir(dir), nil
}
