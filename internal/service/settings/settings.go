// Package settings holds krabby's runtime-mutable docs/RAG configuration.
//
// File/env config (see internal/config) seeds this store on first run; from then
// on the persisted record is authoritative and can be changed live via MCP tools
// or the REST API. Secrets (API keys) are write-only: they persist but are never
// returned; a redacted view exposes only "*_key_set" booleans, and an empty
// secret on update means "keep the existing value".
package settings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rakunlabs/bw"
)

// recordID is the single-row key for the settings bucket.
const recordID = "docs"

// Settings is the mutable docs/RAG configuration.
type Settings struct {
	ID string `bw:"id,pk" json:"-"` // always recordID

	// Docs (generation).
	DocsEnabled     bool     `bw:"docs_enabled"     json:"docs_enabled"`
	DocsConcurrency int      `bw:"docs_concurrency" json:"docs_concurrency"`
	DocsInclude     []string `bw:"docs_include"     json:"docs_include"`
	DocsExclude     []string `bw:"docs_exclude"     json:"docs_exclude"`

	// LLM (chat) for doc generation.
	LLMBaseURL string        `bw:"llm_base_url" json:"llm_base_url"`
	LLMAPIKey  string        `bw:"llm_api_key"  json:"-"` // write-only
	LLMModel   string        `bw:"llm_model"    json:"llm_model"`
	LLMTimeout time.Duration `bw:"llm_timeout"  json:"llm_timeout"`

	// Embedder (embeddings) for RAG.
	EmbedBaseURL string        `bw:"embed_base_url" json:"embed_base_url"`
	EmbedAPIKey  string        `bw:"embed_api_key"  json:"-"` // write-only
	EmbedModel   string        `bw:"embed_model"    json:"embed_model"`
	EmbedDim     int           `bw:"embed_dim"      json:"embed_dim"`
	EmbedBatch   int           `bw:"embed_batch"    json:"embed_batch"`
	EmbedTimeout time.Duration `bw:"embed_timeout"  json:"embed_timeout"`

	// RAG (retrieval + store).
	RAGEnabled      bool   `bw:"rag_enabled"       json:"rag_enabled"`
	RAGChunkSize    int    `bw:"rag_chunk_size"    json:"rag_chunk_size"`
	RAGChunkOverlap int    `bw:"rag_chunk_overlap" json:"rag_chunk_overlap"`
	RAGTopK         int    `bw:"rag_top_k"         json:"rag_top_k"`
	RAGTopDocs      int    `bw:"rag_top_docs"      json:"rag_top_docs"`
	StoreKind       string `bw:"store_kind"        json:"store_kind"`

	// Qdrant (used when StoreKind == "qdrant").
	QdrantURL        string `bw:"qdrant_url"        json:"qdrant_url"`
	QdrantAPIKey     string `bw:"qdrant_api_key"   json:"-"` // write-only
	QdrantCollection string `bw:"qdrant_collection" json:"qdrant_collection"`

	UpdatedAt time.Time `bw:"updated_at" json:"updated_at,omitzero"`
}

// Redacted is a safe-to-return view of Settings: secrets are replaced by
// booleans indicating whether each is set.
type Redacted struct {
	Settings
	LLMAPIKeySet    bool `json:"llm_api_key_set"`
	EmbedAPIKeySet  bool `json:"embed_api_key_set"`
	QdrantAPIKeySet bool `json:"qdrant_api_key_set"`
}

// Redact returns a view with secrets removed and "*_set" booleans populated.
func (s Settings) Redact() Redacted {
	r := Redacted{
		Settings:        s,
		LLMAPIKeySet:    s.LLMAPIKey != "",
		EmbedAPIKeySet:  s.EmbedAPIKey != "",
		QdrantAPIKeySet: s.QdrantAPIKey != "",
	}
	// Defensive: ensure the embedded copy carries no secrets (they have json:"-"
	// so they never marshal, but zero them to avoid accidental in-process leaks).
	r.Settings.LLMAPIKey = ""
	r.Settings.EmbedAPIKey = ""
	r.Settings.QdrantAPIKey = ""

	return r
}

// Patch is the JSON-decodable input for updating settings over the REST API.
// Unlike Settings, its secret fields DO decode from JSON (write-only on input);
// they are never present in any response type. Empty secret = keep existing.
type Patch struct {
	DocsEnabled     bool     `json:"docs_enabled"`
	DocsConcurrency int      `json:"docs_concurrency"`
	DocsInclude     []string `json:"docs_include"`
	DocsExclude     []string `json:"docs_exclude"`

	LLMBaseURL string        `json:"llm_base_url"`
	LLMAPIKey  string        `json:"llm_api_key"`
	LLMModel   string        `json:"llm_model"`
	LLMTimeout time.Duration `json:"llm_timeout"`

	EmbedBaseURL string        `json:"embed_base_url"`
	EmbedAPIKey  string        `json:"embed_api_key"`
	EmbedModel   string        `json:"embed_model"`
	EmbedDim     int           `json:"embed_dim"`
	EmbedBatch   int           `json:"embed_batch"`
	EmbedTimeout time.Duration `json:"embed_timeout"`

	RAGEnabled      bool   `json:"rag_enabled"`
	RAGChunkSize    int    `json:"rag_chunk_size"`
	RAGChunkOverlap int    `json:"rag_chunk_overlap"`
	RAGTopK         int    `json:"rag_top_k"`
	RAGTopDocs      int    `json:"rag_top_docs"`
	StoreKind       string `json:"store_kind"`

	QdrantURL        string `json:"qdrant_url"`
	QdrantAPIKey     string `json:"qdrant_api_key"`
	QdrantCollection string `json:"qdrant_collection"`
}

// ToSettings converts a decoded Patch into a Settings value for Store.Set.
func (p Patch) ToSettings() Settings {
	return Settings{
		DocsEnabled:      p.DocsEnabled,
		DocsConcurrency:  p.DocsConcurrency,
		DocsInclude:      p.DocsInclude,
		DocsExclude:      p.DocsExclude,
		LLMBaseURL:       p.LLMBaseURL,
		LLMAPIKey:        p.LLMAPIKey,
		LLMModel:         p.LLMModel,
		LLMTimeout:       p.LLMTimeout,
		EmbedBaseURL:     p.EmbedBaseURL,
		EmbedAPIKey:      p.EmbedAPIKey,
		EmbedModel:       p.EmbedModel,
		EmbedDim:         p.EmbedDim,
		EmbedBatch:       p.EmbedBatch,
		EmbedTimeout:     p.EmbedTimeout,
		RAGEnabled:       p.RAGEnabled,
		RAGChunkSize:     p.RAGChunkSize,
		RAGChunkOverlap:  p.RAGChunkOverlap,
		RAGTopK:          p.RAGTopK,
		RAGTopDocs:       p.RAGTopDocs,
		StoreKind:        p.StoreKind,
		QdrantURL:        p.QdrantURL,
		QdrantAPIKey:     p.QdrantAPIKey,
		QdrantCollection: p.QdrantCollection,
	}
}

// Store persists a single Settings record.
type Store struct {
	bucket *bw.Bucket[Settings]
}

// New opens the settings bucket. If no record exists yet, seed is persisted as
// the initial configuration (seeded from file/env config by the caller).
func New(db *bw.DB, seed Settings) (*Store, error) {
	bucket, err := bw.RegisterBucket[Settings](db, "settings")
	if err != nil {
		return nil, fmt.Errorf("register settings bucket; %w", err)
	}

	s := &Store{bucket: bucket}

	existing, err := s.getRaw(context.Background())
	if err != nil {
		return nil, err
	}

	if existing == nil {
		seed.ID = recordID
		seed.UpdatedAt = time.Now()
		if err := bucket.Insert(context.Background(), &seed); err != nil {
			return nil, fmt.Errorf("seed settings; %w", err)
		}
	}

	return s, nil
}

func (s *Store) getRaw(ctx context.Context) (*Settings, error) {
	rec, err := s.bucket.Get(ctx, recordID)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get settings; %w", err)
	}

	return rec, nil
}

// Get returns the current settings (with secrets, for internal client building).
func (s *Store) Get(ctx context.Context) (Settings, error) {
	rec, err := s.getRaw(ctx)
	if err != nil {
		return Settings{}, err
	}

	if rec == nil {
		return Settings{ID: recordID}, nil
	}

	return *rec, nil
}

// Set merges patch into the current settings and persists the result.
//
// Secret fields (the three API keys) follow keep-if-empty semantics: an empty
// value in patch leaves the stored secret unchanged, so the UI never needs to
// resend secrets. All non-secret fields are taken from patch as-is.
func (s *Store) Set(ctx context.Context, patch Settings) (Settings, error) {
	cur, err := s.Get(ctx)
	if err != nil {
		return Settings{}, err
	}

	next := patch
	next.ID = recordID

	// Keep-if-empty for secrets.
	if next.LLMAPIKey == "" {
		next.LLMAPIKey = cur.LLMAPIKey
	}

	if next.EmbedAPIKey == "" {
		next.EmbedAPIKey = cur.EmbedAPIKey
	}

	if next.QdrantAPIKey == "" {
		next.QdrantAPIKey = cur.QdrantAPIKey
	}

	next.UpdatedAt = time.Now()

	if err := s.bucket.Insert(ctx, &next); err != nil {
		return Settings{}, fmt.Errorf("save settings; %w", err)
	}

	return next, nil
}
