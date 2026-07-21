package vectorstore

import (
	"context"
	"errors"
	"os"
	"sync"
)

// embedded is a file-backed vector store. Vectors are held in memory and
// persisted to vectorsDir; similarity is cosine, computed in Go. It fits
// krabby's zero-infra, plain-files-under-data_dir model for small/medium corpora.
//
// SCAFFOLD: struct + method surface are final; persistence and cosine search
// are stubs.
type embedded struct {
	dir string

	mu    sync.RWMutex
	items map[string]Item // keyed by Item.ID
}

func newEmbedded(dir string) (*embedded, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	s := &embedded{dir: dir, items: map[string]Item{}}

	// TODO(scaffold): load persisted vectors from dir into s.items.

	return s, nil
}

func (s *embedded) Upsert(_ context.Context, items []Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, it := range items {
		s.items[it.ID] = it
	}

	// TODO(scaffold): persist to disk (per-repo file under s.dir).
	return errors.New("embedded.Upsert: persistence not implemented (scaffold)")
}

func (s *embedded) Search(_ context.Context, _ string, _ []float32, _ int) ([]Match, error) {
	// TODO(scaffold): cosine similarity over s.items filtered by repo, top-K heap.
	return nil, errors.New("embedded.Search: not implemented (scaffold)")
}

func (s *embedded) DeleteRepo(_ context.Context, repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, it := range s.items {
		if it.Payload.Repo == repo {
			delete(s.items, id)
		}
	}

	// TODO(scaffold): remove/rewrite the repo's persisted file.
	return errors.New("embedded.DeleteRepo: persistence not implemented (scaffold)")
}

func (s *embedded) Close() error { return nil }
