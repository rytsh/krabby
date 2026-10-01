package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// Configure builds a new docs/RAG client bundle from s and swaps it in
// atomically. On success the previous bundle's resources are closed. On failure
// the previous (working) bundle is left in place and the error is returned so
// the caller (UI/MCP) can surface it.
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

	if cerr := m.bundleState.replace(bundle); cerr != nil {
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

	d, release := m.acquireDocs()
	defer release()

	if d == nil || d.tracer == nil {
		return langfuse.Disabled()
	}

	return d.tracer
}

// buildBundle constructs docgen/rag clients from settings. A disabled or
// unconfigured capability yields a nil field rather than an error, so partial
// configuration (e.g. docs on, rag off) is valid. Store construction failures
// leave the previous live bundle active.
func (m *Manager) buildBundle(s settings.Settings) (_ *docsBundle, err error) {
	prev, release := m.acquireDocs()
	release()

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
			pictureChat, err := llm.New(llmConfig(s), llm.WithTracer(b.tracer), llm.WithResponseLimit(8<<20))
			if err != nil {
				return nil, fmt.Errorf("build big picture llm client; %w", err)
			}
			b.pictureChat = pictureChat
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
