package vectorstore

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

// qdrant is an HTTP-backed vector store for the Qdrant engine. Opt-in via
// rag.store.kind = "qdrant" for larger corpora / shared deployments.
//
// SCAFFOLD: struct + method surface are final; HTTP calls are stubs.
type qdrant struct {
	url        string
	apiKey     string
	collection string
	dim        int
	http       *http.Client
}

func newQdrant(cfg config.Qdrant, dim int) (*qdrant, error) {
	if cfg.URL == "" {
		return nil, errors.New("qdrant url is required")
	}

	q := &qdrant{
		url:        cfg.URL,
		apiKey:     cfg.APIKey,
		collection: cfg.Collection,
		dim:        dim,
		http:       &http.Client{Timeout: 30 * time.Second},
	}

	// TODO(scaffold): ensure collection exists (PUT /collections/{c}) with the
	// right vector size (dim) and distance (Cosine).

	return q, nil
}

func (q *qdrant) Upsert(_ context.Context, _ []Item) error {
	// TODO(scaffold): PUT /collections/{c}/points with points{id,vector,payload}.
	return errors.New("qdrant.Upsert: not implemented (scaffold)")
}

func (q *qdrant) Search(_ context.Context, _ string, _ []float32, _ int) ([]Match, error) {
	// TODO(scaffold): POST /collections/{c}/points/search with vector, limit,
	// and a payload filter on repo when set.
	return nil, errors.New("qdrant.Search: not implemented (scaffold)")
}

func (q *qdrant) DeleteRepo(_ context.Context, _ string) error {
	// TODO(scaffold): POST /collections/{c}/points/delete with a repo filter.
	return errors.New("qdrant.DeleteRepo: not implemented (scaffold)")
}

func (q *qdrant) Close() error { return nil }
