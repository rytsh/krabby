// Package rag wires the documentation markdown, the embedder and the vector
// store into an indexing + retrieval service.
//
// Retrieval is file-level: the vector index only routes a question to the most
// relevant markdown documents; the WHOLE markdown file(s) are then returned to
// the caller/LLM (not just the matching chunks).
//
// SCAFFOLD: types and the Service surface are final; Index/Retrieve bodies are
// stubs.
package rag

import (
	"context"
	"errors"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// Doc is a whole markdown document returned by retrieval.
type Doc struct {
	Repo    string  `json:"repo"`
	Path    string  `json:"path"`  // repo-relative path under krabby-docs/
	Title   string  `json:"title"`
	Score   float32 `json:"score"` // best chunk score that surfaced this doc
	Content string  `json:"content"`
}

// Service indexes generated docs and retrieves whole docs for a question.
type Service struct {
	cfg   config.RAG
	emb   *embedder.Client
	store vectorstore.Store
}

// New builds a RAG service. emb and store must be non-nil; callers gate on
// rag.enabled + configured embedder/store before constructing.
func New(cfg config.RAG, emb *embedder.Client, store vectorstore.Store) *Service {
	return &Service{cfg: cfg, emb: emb, store: store}
}

// Index (re)builds the vector index for a repo's generated docs. It reads the
// markdown files under docsDir, chunks them, embeds the chunks and upserts them
// into the store (replacing any prior vectors for the repo).
//
// TODO(scaffold):
//   1. store.DeleteRepo(repo)  (full rebuild) or diff via manifest (incremental)
//   2. for each doc: chunk (heading-aware, size-capped) -> texts
//   3. emb.Embed(texts) -> vectors
//   4. store.Upsert(items{ID: repo+path+idx, Vector, Payload{repo,path,title,chunk}})
func (s *Service) Index(_ context.Context, _ string, _ string) error {
	return errors.New("rag.Index: not implemented (scaffold)")
}

// DeleteRepo removes a repo's vectors from the index (on repo removal).
func (s *Service) DeleteRepo(ctx context.Context, repo string) error {
	return s.store.DeleteRepo(ctx, repo)
}

// Retrieve returns up to topDocs whole markdown documents most relevant to the
// question. repo == "" searches across all repos. topDocs <= 0 uses the
// configured default (RAG.TopDocs).
//
// TODO(scaffold):
//   1. emb.Embed([question]) -> qvec
//   2. store.Search(repo, qvec, cfg.TopK) -> chunk matches
//   3. group matches by DocPath, doc score = max chunk score
//   4. take top N doc paths, read the WHOLE markdown file for each
//   5. return []Doc sorted by score
func (s *Service) Retrieve(_ context.Context, _ string, _ string, _ int) ([]Doc, error) {
	return nil, errors.New("rag.Retrieve: not implemented (scaffold)")
}
