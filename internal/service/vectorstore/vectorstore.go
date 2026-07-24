// Package vectorstore defines krabby's embedded bw vector index.
package vectorstore

import (
	"context"
	"time"
)

// Payload is the metadata carried with each stored vector. It is enough to
// locate and display the source document without re-reading the index.
//
// Docs RAG fills Repo/DocPath/Title/Chunk. Code RAG additionally fills
// Symbol/StartLine/EndLine (DocPath is then the repo-relative source path).
type Payload struct {
	Repo    string `json:"repo"`     // owner/name
	DocPath string `json:"doc_path"` // repo-relative markdown or source path
	Title   string `json:"title"`
	Chunk   string `json:"chunk"` // the chunk text

	// UpdatedAt is the source document's last-modified time (JIRA "updated",
	// Confluence version.when), when known. It lets retrieval surface recency to
	// the model and apply a mild recency bias so a stale item does not outrank a
	// fresh, similarly-relevant one. Zero when the source has no such timestamp.
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	Symbol    string `json:"symbol,omitempty"`     // code: leading symbol in the chunk
	StartLine int    `json:"start_line,omitempty"` // code: 1-based inclusive
	EndLine   int    `json:"end_line,omitempty"`   // code: 1-based inclusive
}

// Item is a vector plus its payload, keyed by a stable ID.
type Item struct {
	ID      string    `json:"id"` // stable: repo + docPath + chunkIdx
	Vector  []float32 `json:"vector"`
	Payload Payload   `json:"payload"`
}

// Match is a search hit with a similarity score (higher = closer).
type Match struct {
	Payload Payload `json:"payload"`
	Score   float32 `json:"score"`
}

// Filter restricts a search to a subset of the indexed keys (repo ids or
// web-source scope keys). The zero value matches everything. All set fields
// are combined with AND.
type Filter struct {
	// Keys restricts matches to these exact keys.
	Keys []string
	// Prefix restricts matches to keys starting with this prefix (e.g. the
	// web-source namespace "web:").
	Prefix string
	// ExcludePrefix drops keys starting with this prefix.
	ExcludePrefix string
}

// FilterKey builds a single-key filter; an empty key matches everything.
func FilterKey(key string) Filter {
	if key == "" {
		return Filter{}
	}

	return Filter{Keys: []string{key}}
}

// IsZero reports whether the filter matches everything.
func (f Filter) IsZero() bool {
	return len(f.Keys) == 0 && f.Prefix == "" && f.ExcludePrefix == ""
}

// Store is the vector index used by docs and code RAG.
type Store interface {
	// Upsert inserts or replaces the given items. IDs are stable so re-indexing
	// a doc overwrites its prior chunks.
	Upsert(ctx context.Context, items []Item) error
	// Search returns the topK nearest chunks whose key matches the filter.
	Search(ctx context.Context, filter Filter, vec []float32, topK int) ([]Match, error)
	// DeleteRepo removes all vectors belonging to a repo.
	DeleteRepo(ctx context.Context, repo string) error
	// HasRepo reports whether the index holds at least one vector for the
	// repo. Used to detect a missing/empty index so callers can force a
	// rebuild even when higher-level stage state claims success.
	HasRepo(ctx context.Context, repo string) (bool, error)
	// IndexedPaths returns the distinct payload DocPaths that have at least one
	// vector for the repo. Used to reconcile the index against the docs on disk
	// so pages whose markdown exists but whose vectors are missing (e.g. an
	// interrupted embed run) are re-embedded on the next sync.
	IndexedPaths(ctx context.Context, repo string) (map[string]struct{}, error)
	// DeletePaths removes a repo's vectors whose payload DocPath is in paths.
	// Used for incremental re-indexing of changed/deleted files.
	DeletePaths(ctx context.Context, repo string, paths []string) error
	// Close flushes and releases resources.
	Close() error
}

// New opens the embedded bw vector store at dir.
func New(dir string) (Store, error) { return newEmbedded(dir) }
