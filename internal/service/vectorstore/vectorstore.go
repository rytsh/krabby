// Package vectorstore defines the pluggable vector index behind krabby's RAG
// layer. The default backend is embedded (file-backed, cosine similarity in Go);
// a Qdrant HTTP backend is available for scale. Additional backends (pgvector,
// etc.) can implement the Store interface.
package vectorstore

import (
	"context"
	"fmt"

	"github.com/rytsh/krabby/internal/config"
)

// Payload is the metadata carried with each stored vector. It is enough to
// locate and display the source document without re-reading the index.
type Payload struct {
	Repo    string `json:"repo"`     // owner/name
	DocPath string `json:"doc_path"` // repo-relative markdown path under krabby-docs/
	Title   string `json:"title"`
	Chunk   string `json:"chunk"` // the chunk text
}

// Item is a vector plus its payload, keyed by a stable ID.
type Item struct {
	ID      string  `json:"id"` // stable: repo + docPath + chunkIdx
	Vector  []float32 `json:"vector"`
	Payload Payload `json:"payload"`
}

// Match is a search hit with a similarity score (higher = closer).
type Match struct {
	Payload Payload `json:"payload"`
	Score   float32 `json:"score"`
}

// Store is the pluggable vector index.
type Store interface {
	// Upsert inserts or replaces the given items. IDs are stable so re-indexing
	// a doc overwrites its prior chunks.
	Upsert(ctx context.Context, items []Item) error
	// Search returns the topK nearest chunks. repo == "" searches all repos;
	// otherwise results are restricted to that repo.
	Search(ctx context.Context, repo string, vec []float32, topK int) ([]Match, error)
	// DeleteRepo removes all vectors belonging to a repo.
	DeleteRepo(ctx context.Context, repo string) error
	// Close flushes and releases resources.
	Close() error
}

// New builds the configured vector store. dim is the embedding dimension (used
// by backends that require it up front, e.g. Qdrant collection creation).
func New(cfg config.RAG, vectorsDir string, dim int) (Store, error) {
	switch cfg.Store.Kind {
	case "", "embedded":
		return newEmbedded(vectorsDir)
	case "qdrant":
		return newQdrant(cfg.Store.Qdrant, dim)
	default:
		return nil, fmt.Errorf("unknown vector store kind %q", cfg.Store.Kind)
	}
}
