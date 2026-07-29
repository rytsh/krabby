// Package vectorstore defines krabby's embedded bw vector index.
package vectorstore

import (
	"context"
	"strings"
	"time"

	"github.com/rakunlabs/query"
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

// ScopePrefix namespaces web-source keys in the shared docs index. Repo ids
// can never contain ':' so the two key spaces cannot collide.
//
// The convention belongs to the store because the store is what has to answer
// questions about it; websource re-exports this constant.
const ScopePrefix = "web:"

// The two classes of key a stored chunk can belong to. They are persisted in
// an indexed field rather than derived from the key at query time, so asking
// for one class is an index seek instead of a scan.
const (
	KindRepo = "repo"
	KindWeb  = "web"
)

// KindOf classifies a store key.
func KindOf(key string) string {
	if strings.HasPrefix(key, ScopePrefix) {
		return KindWeb
	}

	return KindRepo
}

// Filter restricts a search to a subset of the indexed keys (repo ids or
// web-source scope keys). The zero value matches everything. All set fields
// are combined with AND.
type Filter struct {
	// Keys restricts matches to these exact keys.
	Keys []string
	// Kind restricts matches to one class of key: KindRepo or KindWeb.
	//
	// This used to be a pair of prefix predicates on the key itself, which
	// read naturally but could not be served by an index: bw's planner has no
	// plan for LIKE / NOT LIKE, so a scope search degraded into a scan that
	// decoded every chunk of every repo and every source. Matching a stored
	// discriminator instead keeps it an index seek.
	Kind string
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
	return len(f.Keys) == 0 && f.Kind == ""
}

// Query translates the filter into a bw where clause over the indexed "repo"
// field, or nil when it matches everything. Both index backends (vectors and
// the lexical text index) push the same filter down to bw, so the two arms of a
// hybrid search agree on what is in scope.
func (f Filter) Query() *query.Query {
	if f.IsZero() {
		return nil
	}

	q := query.New()

	switch len(f.Keys) {
	case 0:
	case 1:
		q.Where = append(q.Where,
			query.NewExpressionCmp(query.OperatorEq, "repo", f.Keys[0]).Expression())
	default:
		q.Where = append(q.Where,
			query.NewExpressionCmp(query.OperatorIn, "repo", f.Keys).Expression())
	}

	if f.Kind != "" {
		q.Where = append(q.Where,
			query.NewExpressionCmp(query.OperatorEq, "kind", f.Kind).Expression())
	}

	return q
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
