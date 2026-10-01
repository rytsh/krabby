package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// docsBundle is an immutable snapshot of the docs/RAG clients. A nil field means
// that capability is disabled. Bundles are swapped atomically by Configure; the
// previous bundle's owned resources are closed after a swap.
type docsBundle struct {
	gen         docgen.Generator
	pictureChat bigpicture.Completer
	rag         *rag.Service
	store       vectorstore.Store // owned; closed on swap
	vision      *llm.Client
	imageCfg    config.WebImage

	codeRag   *coderag.Service
	codeStore vectorstore.Store // owned; closed on swap

	// tracer exports LLM activity to Langfuse. Never nil: a disabled export
	// is an inert tracer, so call sites need no guard. It is owned by the
	// bundle and shut down on swap, but only when the replacement was built
	// from a different configuration - see buildBundle.
	tracer         *langfuse.Tracer
	tracerShutdown func(context.Context) error

	// ragCfg carries the docs retrieval tuning (hybrid fusion, lexical query
	// building). It is kept on the bundle rather than read from rag because
	// lexical-only search must stay configurable when rag is nil.
	ragCfg config.RAG
}

// acquireDocs leases the active bundle until the returned release function is
// called. Configure waits for all leases before closing replaced stores, so an
// in-flight search/index can never race a live settings update.
func (m *Manager) acquireDocs() (*docsBundle, func()) {
	return m.bundleState.acquire()
}

// bundleState retains the existing lease policy: a replacement waits until
// all readers release the old bundle. Writers are serialized by configureMu
// at the manager boundary, including shutdown and resource ownership transfer.
type bundleState struct {
	docsMu sync.RWMutex
	docs   *docsBundle
}

func (s *bundleState) acquire() (*docsBundle, func()) {
	s.docsMu.RLock()
	return s.docs, s.docsMu.RUnlock
}

func (s *bundleState) replace(next *docsBundle) error {
	s.docsMu.Lock()
	prev := s.docs
	s.docs = next
	s.docsMu.Unlock()
	return prev.closeExcept(next)
}

// tracerShutdownTimeout bounds the final flush of a replaced tracer.
const tracerShutdownTimeout = 5 * time.Second

// closeExcept releases resources not transferred to the replacement. Pointer
// identity preserves borrowed stores and tracers during both swap and rollback.
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
