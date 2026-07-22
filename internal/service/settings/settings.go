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
	DocsEnabled      bool     `bw:"docs_enabled"      json:"docs_enabled"`
	DocsConcurrency  int      `bw:"docs_concurrency"  json:"docs_concurrency"`
	DocsSummaryModel string   `bw:"docs_summary_model" json:"docs_summary_model"`
	DocsMaxGroups    int      `bw:"docs_max_groups"   json:"docs_max_groups"`
	DocsInclude      []string `bw:"docs_include"      json:"docs_include"`
	DocsExclude      []string `bw:"docs_exclude"      json:"docs_exclude"`
	DocsPrompt       string   `bw:"docs_prompt"       json:"docs_prompt"`

	// LLM (chat) for doc generation.
	LLMBaseURL string        `bw:"llm_base_url" json:"llm_base_url"`
	LLMAPIKey  string        `bw:"llm_api_key"  json:"-"` // write-only
	LLMModel   string        `bw:"llm_model"    json:"llm_model"`
	LLMTimeout time.Duration `bw:"llm_timeout"  json:"llm_timeout"`

	// Embedder (embeddings) for RAG.
	EmbedBaseURL     string        `bw:"embed_base_url"    json:"embed_base_url"`
	EmbedAPIKey      string        `bw:"embed_api_key"     json:"-"` // write-only
	EmbedModel       string        `bw:"embed_model"       json:"embed_model"`
	EmbedDim         int           `bw:"embed_dim"         json:"embed_dim"`
	EmbedBatch       int           `bw:"embed_batch"       json:"embed_batch"`
	EmbedConcurrency int           `bw:"embed_concurrency" json:"embed_concurrency"`
	EmbedTimeout     time.Duration `bw:"embed_timeout"     json:"embed_timeout"`

	// RAG (retrieval over the embedded vector store).
	RAGEnabled      bool `bw:"rag_enabled"       json:"rag_enabled"`
	RAGChunkSize    int  `bw:"rag_chunk_size"    json:"rag_chunk_size"`
	RAGChunkOverlap int  `bw:"rag_chunk_overlap" json:"rag_chunk_overlap"`
	RAGTopK         int  `bw:"rag_top_k"         json:"rag_top_k"`
	RAGTopDocs      int  `bw:"rag_top_docs"      json:"rag_top_docs"`

	// Code embedder (embeddings) for code RAG. When BaseURL is empty the docs
	// embedder settings above are used for code as well.
	CodeEmbedBaseURL     string        `bw:"code_embed_base_url"    json:"code_embed_base_url"`
	CodeEmbedAPIKey      string        `bw:"code_embed_api_key"     json:"-"` // write-only
	CodeEmbedModel       string        `bw:"code_embed_model"       json:"code_embed_model"`
	CodeEmbedDim         int           `bw:"code_embed_dim"         json:"code_embed_dim"`
	CodeEmbedBatch       int           `bw:"code_embed_batch"       json:"code_embed_batch"`
	CodeEmbedConcurrency int           `bw:"code_embed_concurrency" json:"code_embed_concurrency"`
	CodeEmbedTimeout     time.Duration `bw:"code_embed_timeout"     json:"code_embed_timeout"`

	// Code RAG (source-code semantic search).
	CodeRAGEnabled      bool     `bw:"code_rag_enabled"       json:"code_rag_enabled"`
	CodeRAGChunkSize    int      `bw:"code_rag_chunk_size"    json:"code_rag_chunk_size"`
	CodeRAGChunkOverlap int      `bw:"code_rag_chunk_overlap" json:"code_rag_chunk_overlap"`
	CodeRAGTopK         int      `bw:"code_rag_top_k"         json:"code_rag_top_k"`
	CodeRAGInclude      []string `bw:"code_rag_include"       json:"code_rag_include"`
	CodeRAGExclude      []string `bw:"code_rag_exclude"       json:"code_rag_exclude"`

	UpdatedAt time.Time `bw:"updated_at" json:"updated_at,omitzero"`
}

// Redacted is a safe-to-return view of Settings: secrets are replaced by
// booleans indicating whether each is set.
type Redacted struct {
	Settings
	DocsDefaultPrompt  string `json:"docs_default_prompt"`
	LLMAPIKeySet       bool   `json:"llm_api_key_set"`
	EmbedAPIKeySet     bool   `json:"embed_api_key_set"`
	CodeEmbedAPIKeySet bool   `json:"code_embed_api_key_set"`
}

// Redact returns a view with secrets removed and "*_set" booleans populated.
func (s Settings) Redact() Redacted {
	r := Redacted{
		Settings:           s,
		LLMAPIKeySet:       s.LLMAPIKey != "",
		EmbedAPIKeySet:     s.EmbedAPIKey != "",
		CodeEmbedAPIKeySet: s.CodeEmbedAPIKey != "",
	}
	// Defensive: ensure the embedded copy carries no secrets (they have json:"-"
	// so they never marshal, but zero them to avoid accidental in-process leaks).
	r.Settings.LLMAPIKey = ""
	r.Settings.EmbedAPIKey = ""
	r.Settings.CodeEmbedAPIKey = ""

	return r
}

// Patch is the JSON-decodable input for updating settings over the REST API.
// Unlike Settings, its secret fields DO decode from JSON (write-only on input);
// they are never present in any response type. Empty secret = keep existing.
type Patch struct {
	DocsEnabled      *bool     `json:"docs_enabled"`
	DocsConcurrency  *int      `json:"docs_concurrency"`
	DocsSummaryModel *string   `json:"docs_summary_model"`
	DocsMaxGroups    *int      `json:"docs_max_groups"`
	DocsInclude      *[]string `json:"docs_include"`
	DocsExclude      *[]string `json:"docs_exclude"`
	DocsPrompt       *string   `json:"docs_prompt"`

	LLMBaseURL *string        `json:"llm_base_url"`
	LLMAPIKey  *string        `json:"llm_api_key"`
	LLMModel   *string        `json:"llm_model"`
	LLMTimeout *time.Duration `json:"llm_timeout"`

	EmbedBaseURL     *string        `json:"embed_base_url"`
	EmbedAPIKey      *string        `json:"embed_api_key"`
	EmbedModel       *string        `json:"embed_model"`
	EmbedDim         *int           `json:"embed_dim"`
	EmbedBatch       *int           `json:"embed_batch"`
	EmbedConcurrency *int           `json:"embed_concurrency"`
	EmbedTimeout     *time.Duration `json:"embed_timeout"`

	RAGEnabled      *bool `json:"rag_enabled"`
	RAGChunkSize    *int  `json:"rag_chunk_size"`
	RAGChunkOverlap *int  `json:"rag_chunk_overlap"`
	RAGTopK         *int  `json:"rag_top_k"`
	RAGTopDocs      *int  `json:"rag_top_docs"`

	CodeEmbedBaseURL     *string        `json:"code_embed_base_url"`
	CodeEmbedAPIKey      *string        `json:"code_embed_api_key"`
	CodeEmbedModel       *string        `json:"code_embed_model"`
	CodeEmbedDim         *int           `json:"code_embed_dim"`
	CodeEmbedBatch       *int           `json:"code_embed_batch"`
	CodeEmbedConcurrency *int           `json:"code_embed_concurrency"`
	CodeEmbedTimeout     *time.Duration `json:"code_embed_timeout"`

	CodeRAGEnabled      *bool     `json:"code_rag_enabled"`
	CodeRAGChunkSize    *int      `json:"code_rag_chunk_size"`
	CodeRAGChunkOverlap *int      `json:"code_rag_chunk_overlap"`
	CodeRAGTopK         *int      `json:"code_rag_top_k"`
	CodeRAGInclude      *[]string `json:"code_rag_include"`
	CodeRAGExclude      *[]string `json:"code_rag_exclude"`
}

// Apply overlays fields present in p onto base. Pointer fields distinguish an
// omitted JSON property from an explicit zero/false/empty value.
func (p Patch) Apply(base Settings) Settings {
	if p.DocsEnabled != nil {
		base.DocsEnabled = *p.DocsEnabled
	}
	if p.DocsConcurrency != nil {
		base.DocsConcurrency = *p.DocsConcurrency
	}
	if p.DocsSummaryModel != nil {
		base.DocsSummaryModel = *p.DocsSummaryModel
	}
	if p.DocsMaxGroups != nil {
		base.DocsMaxGroups = *p.DocsMaxGroups
	}
	if p.DocsInclude != nil {
		base.DocsInclude = *p.DocsInclude
	}
	if p.DocsExclude != nil {
		base.DocsExclude = *p.DocsExclude
	}
	if p.DocsPrompt != nil {
		base.DocsPrompt = *p.DocsPrompt
	}
	if p.LLMBaseURL != nil {
		base.LLMBaseURL = *p.LLMBaseURL
	}
	if p.LLMAPIKey != nil {
		base.LLMAPIKey = *p.LLMAPIKey
	}
	if p.LLMModel != nil {
		base.LLMModel = *p.LLMModel
	}
	if p.LLMTimeout != nil {
		base.LLMTimeout = *p.LLMTimeout
	}
	if p.EmbedBaseURL != nil {
		base.EmbedBaseURL = *p.EmbedBaseURL
	}
	if p.EmbedAPIKey != nil {
		base.EmbedAPIKey = *p.EmbedAPIKey
	}
	if p.EmbedModel != nil {
		base.EmbedModel = *p.EmbedModel
	}
	if p.EmbedDim != nil {
		base.EmbedDim = *p.EmbedDim
	}
	if p.EmbedBatch != nil {
		base.EmbedBatch = *p.EmbedBatch
	}
	if p.EmbedConcurrency != nil {
		base.EmbedConcurrency = *p.EmbedConcurrency
	}
	if p.EmbedTimeout != nil {
		base.EmbedTimeout = *p.EmbedTimeout
	}
	if p.RAGEnabled != nil {
		base.RAGEnabled = *p.RAGEnabled
	}
	if p.RAGChunkSize != nil {
		base.RAGChunkSize = *p.RAGChunkSize
	}
	if p.RAGChunkOverlap != nil {
		base.RAGChunkOverlap = *p.RAGChunkOverlap
	}
	if p.RAGTopK != nil {
		base.RAGTopK = *p.RAGTopK
	}
	if p.RAGTopDocs != nil {
		base.RAGTopDocs = *p.RAGTopDocs
	}
	if p.CodeEmbedBaseURL != nil {
		base.CodeEmbedBaseURL = *p.CodeEmbedBaseURL
	}
	if p.CodeEmbedAPIKey != nil {
		base.CodeEmbedAPIKey = *p.CodeEmbedAPIKey
	}
	if p.CodeEmbedModel != nil {
		base.CodeEmbedModel = *p.CodeEmbedModel
	}
	if p.CodeEmbedDim != nil {
		base.CodeEmbedDim = *p.CodeEmbedDim
	}
	if p.CodeEmbedBatch != nil {
		base.CodeEmbedBatch = *p.CodeEmbedBatch
	}
	if p.CodeEmbedConcurrency != nil {
		base.CodeEmbedConcurrency = *p.CodeEmbedConcurrency
	}
	if p.CodeEmbedTimeout != nil {
		base.CodeEmbedTimeout = *p.CodeEmbedTimeout
	}
	if p.CodeRAGEnabled != nil {
		base.CodeRAGEnabled = *p.CodeRAGEnabled
	}
	if p.CodeRAGChunkSize != nil {
		base.CodeRAGChunkSize = *p.CodeRAGChunkSize
	}
	if p.CodeRAGChunkOverlap != nil {
		base.CodeRAGChunkOverlap = *p.CodeRAGChunkOverlap
	}
	if p.CodeRAGTopK != nil {
		base.CodeRAGTopK = *p.CodeRAGTopK
	}
	if p.CodeRAGInclude != nil {
		base.CodeRAGInclude = *p.CodeRAGInclude
	}
	if p.CodeRAGExclude != nil {
		base.CodeRAGExclude = *p.CodeRAGExclude
	}
	return base
}

// Store persists a single Settings record.
// MCPKey is the runtime override for the MCP endpoint API key. When a record
// exists it is authoritative (an empty Key means the endpoint is open); when
// absent the file/env config value applies.
type MCPKey struct {
	ID        string    `bw:"id,pk"      json:"-"`
	Key       string    `bw:"key"        json:"-"`
	UpdatedAt time.Time `bw:"updated_at" json:"updated_at"`
}

type Store struct {
	bucket    *bw.Bucket[Settings]
	mcpBucket *bw.Bucket[MCPKey]
}

// settingsSchemaVersion v5 adds docs_summary_model (a fast model for the
// per-file summary phase). v4 added docs_max_groups (community-based doc
// summarization); v3 added embed_concurrency / code_embed_concurrency. Bumping
// the version lets bw migrate existing settings records in place.
const settingsSchemaVersion = 5

// New opens the settings bucket. If no record exists yet, seed is persisted as
// the initial configuration (seeded from file/env config by the caller).
func New(db *bw.DB, seed Settings) (*Store, error) {
	bucket, err := bw.RegisterBucket[Settings](db, "settings", bw.WithVersion[Settings](settingsSchemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register settings bucket; %w", err)
	}

	mcpBucket, err := bw.RegisterBucket[MCPKey](db, "mcp_key")
	if err != nil {
		return nil, fmt.Errorf("register mcp_key bucket; %w", err)
	}

	s := &Store{bucket: bucket, mcpBucket: mcpBucket}

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

	if next.CodeEmbedAPIKey == "" {
		next.CodeEmbedAPIKey = cur.CodeEmbedAPIKey
	}

	next.UpdatedAt = time.Now()

	if err := s.bucket.Insert(ctx, &next); err != nil {
		return Settings{}, fmt.Errorf("save settings; %w", err)
	}

	return next, nil
}

// MCPKey returns the runtime MCP key override, or nil when none is stored.
func (s *Store) MCPKey(ctx context.Context) (*MCPKey, error) {
	rec, err := s.mcpBucket.Get(ctx, recordID)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get mcp key; %w", err)
	}

	return rec, nil
}

// SetMCPKey stores the runtime MCP key override. An empty key is a valid
// override meaning "no auth".
func (s *Store) SetMCPKey(ctx context.Context, key string) error {
	rec := &MCPKey{ID: recordID, Key: key, UpdatedAt: time.Now()}
	if err := s.mcpBucket.Insert(ctx, rec); err != nil {
		return fmt.Errorf("save mcp key; %w", err)
	}

	return nil
}

// ClearMCPKey removes the runtime override so the file/env config value
// applies again.
func (s *Store) ClearMCPKey(ctx context.Context) error {
	if err := s.mcpBucket.Delete(ctx, recordID); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("clear mcp key; %w", err)
	}

	return nil
}
