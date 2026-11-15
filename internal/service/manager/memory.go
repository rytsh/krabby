package manager

import (
	"context"

	"github.com/rytsh/krabby/internal/service/memory"
	"github.com/rytsh/krabby/internal/service/memorysvc"
)

// SetMemories installs the memory store and builds the memory service over
// it. The service reaches the rest of the manager only through the
// dependencies wired here.
func (m *Manager) SetMemories(store *memory.Store) {
	m.memories = memorysvc.New(store, memorysvc.Deps{
		ResolveRepo:    m.reg.Resolve,
		Repo:           m.reg.Get,
		ReadRepoFile:   m.ReadRepoFile,
		FindDefinition: m.FindDefinition,
		Embed:          m.embedForMemory,
		Go:             m.goBackground,
	})
}

// Memories returns the memory service, which serves the memory MCP catalog
// and the memory REST routes. It is nil until SetMemories, and a nil service
// answers every request with memorysvc.ErrUnavailable.
func (m *Manager) Memories() *memorysvc.Service { return m.memories }

// embedForMemory embeds text with the docs embedder, which memories share. It
// returns a nil vector when no embedder is configured.
func (m *Manager) embedForMemory(ctx context.Context, text string, query bool) ([]float32, string, error) {
	d, release := m.acquireDocs()
	defer release()

	if d == nil || d.rag == nil {
		return nil, "", nil
	}
	emb := d.rag.Embedder()
	if emb == nil {
		return nil, "", nil
	}

	if query {
		vec, err := emb.EmbedQuery(ctx, text)

		return vec, emb.Model(), err
	}

	vecs, err := emb.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		return nil, "", err
	}

	return vecs[0], emb.Model(), nil
}

// goBackground runs fn under the manager's lifecycle: Close waits for it, and
// it is refused once shutdown has begun.
func (m *Manager) goBackground(fn func(ctx context.Context)) bool {
	if !m.startWork() {
		return false
	}

	go func() {
		defer m.wg.Done()
		fn(m.baseCtx)
	}()

	return true
}
