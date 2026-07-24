// Package rag wires the documentation markdown, the embedder and the vector
// store into an indexing + retrieval service.
//
// Retrieval returns bounded excerpts from the most relevant markdown documents.
package rag

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

const (
	DefaultTopDocs  = 3
	MaxTopDocs      = 5
	MaxExcerptRunes = 4000
)

// Doc is a ranked documentation excerpt returned by retrieval.
type Doc struct {
	Repo      string  `json:"repo"`
	Path      string  `json:"path"` // path relative to the repo's docs directory
	Title     string  `json:"title"`
	Score     float32 `json:"score"` // recency-adjusted score used for ranking
	Excerpt   string  `json:"excerpt"`
	Truncated bool    `json:"truncated,omitempty"`

	// UpdatedAt is the source document's last-modified time, when known, so the
	// model can judge how current a hit is (a decade-old ticket vs. a fresh
	// one). Zero/omitted when the source has no timestamp.
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	// URL and Teams are populated only for web-source hits (e.g. JIRA
	// tickets): URL is the original item link, Teams the owning team names.
	URL   string   `json:"url,omitempty"`
	Teams []string `json:"teams,omitempty"`
}

// Service indexes generated docs and retrieves bounded excerpts for a question.
type Service struct {
	cfg   config.RAG
	emb   *embedder.Client
	store vectorstore.Store
}

// New builds a RAG service. emb and store must be non-nil.
func New(cfg config.RAG, emb *embedder.Client, store vectorstore.Store) *Service {
	return &Service{cfg: cfg, emb: emb, store: store}
}

// Index (re)builds the vector index for a repo's generated docs. It reads the
// markdown files under docsDir, chunks them (heading-aware, size-capped),
// embeds the chunks and upserts them into the store, replacing any prior
// vectors for the repo.
func (s *Service) Index(ctx context.Context, repo string, docsDir string) error {
	titles := manifestTitles(docsDir)

	var (
		items []vectorstore.Item
		texts []string
	)

	err := filepath.WalkDir(docsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		rel, err := filepath.Rel(docsDir, path)
		if err != nil {
			return err
		}

		docPath := filepath.ToSlash(rel)

		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		content := string(b)

		title := titles[docPath]
		if title == "" {
			title = firstHeading(content)
		}

		if title == "" {
			title = docPath
		}

		for i, c := range chunk(content, s.cfg.ChunkSize, s.cfg.ChunkOverlap) {
			items = append(items, vectorstore.Item{
				ID: fmt.Sprintf("%s/%s#%d", repo, docPath, i),
				Payload: vectorstore.Payload{
					Repo:    repo,
					DocPath: docPath,
					Title:   title,
					Chunk:   c,
				},
			})
			texts = append(texts, c)
		}

		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("docs dir %s does not exist; generate docs first", docsDir)
		}

		return fmt.Errorf("walk docs dir; %w", err)
	}

	if len(items) == 0 {
		// No docs -> make the index match (empty).
		return s.store.DeleteRepo(ctx, repo)
	}

	vecs, err := s.emb.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed %d chunks; %w", len(texts), err)
	}

	if len(vecs) != len(items) {
		return fmt.Errorf("embedder returned %d vectors for %d chunks", len(vecs), len(items))
	}

	for i := range items {
		items[i].Vector = vecs[i]
	}

	// Full rebuild: drop prior vectors so removed docs disappear, then upsert.
	if err := s.store.DeleteRepo(ctx, repo); err != nil {
		return fmt.Errorf("clear prior vectors; %w", err)
	}

	if err := s.store.Upsert(ctx, items); err != nil {
		return fmt.Errorf("upsert %d vectors; %w", len(items), err)
	}

	slog.Info("rag index rebuilt", "repo", repo, "docs", len(titlesIndexed(items)), "chunks", len(items))

	return nil
}

// IndexPaths incrementally updates a repo's docs vectors: it re-embeds only the
// markdown files in changed (relative, slash-separated, e.g. "ofs-1.md") and
// removes vectors for changed+removed paths first, leaving every other doc's
// vectors untouched. This avoids re-embedding an entire large collection (e.g.
// a JIRA project) when only a few items changed. A changed path whose file is
// missing on disk is treated as removed.
func (s *Service) IndexPaths(ctx context.Context, repo, docsDir string, changed, removed []string) error {
	return s.IndexPathsProgress(ctx, repo, docsDir, changed, removed, nil, nil)
}

// IndexOptions carries optional per-index inputs. updatedAt, when set, returns
// a doc's source last-modified time for a given path (slash-separated, e.g.
// "ofs-1.md"); the returned time is stored on every chunk of that doc so
// retrieval can surface and weigh recency. A nil func or zero time leaves the
// timestamp empty.
type IndexOptions struct {
	UpdatedAt func(path string) time.Time
}

// IndexPathsProgress is IndexPaths with an optional progress callback (invoked
// as chunks are embedded, done out of total, for a determinate progress bar)
// and optional per-doc metadata (opts). Both may be nil. The progress callback
// may run concurrently.
func (s *Service) IndexPathsProgress(ctx context.Context, repo, docsDir string, changed, removed []string, onProgress func(done, total int), opts *IndexOptions) error {
	// Drop prior vectors for everything we are about to touch so stale chunks
	// (including those of now-removed docs) disappear.
	stale := make([]string, 0, len(changed)+len(removed))
	stale = append(stale, removed...)
	stale = append(stale, changed...)
	if len(stale) > 0 {
		if err := s.store.DeletePaths(ctx, repo, stale); err != nil {
			return fmt.Errorf("delete changed vectors; %w", err)
		}
	}

	titles := manifestTitles(docsDir)

	var (
		items []vectorstore.Item
		texts []string
	)

	for _, docPath := range changed {
		docPath = filepath.ToSlash(docPath)
		b, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(docPath)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // treated as removed: vectors already dropped above
			}

			return fmt.Errorf("read %s; %w", docPath, err)
		}

		content := string(b)
		title := titles[docPath]
		if title == "" {
			title = firstHeading(content)
		}
		if title == "" {
			title = docPath
		}

		var updatedAt time.Time
		if opts != nil && opts.UpdatedAt != nil {
			updatedAt = opts.UpdatedAt(docPath)
		}

		for i, c := range chunk(content, s.cfg.ChunkSize, s.cfg.ChunkOverlap) {
			items = append(items, vectorstore.Item{
				ID: fmt.Sprintf("%s/%s#%d", repo, docPath, i),
				Payload: vectorstore.Payload{
					Repo:      repo,
					DocPath:   docPath,
					Title:     title,
					Chunk:     c,
					UpdatedAt: updatedAt,
				},
			})
			texts = append(texts, c)
		}
	}

	if len(items) == 0 {
		return nil
	}

	vecs, err := s.emb.EmbedWithProgress(ctx, texts, onProgress)
	if err != nil {
		return fmt.Errorf("embed %d chunks; %w", len(texts), err)
	}
	if len(vecs) != len(items) {
		return fmt.Errorf("embedder returned %d vectors for %d chunks", len(vecs), len(items))
	}
	for i := range items {
		items[i].Vector = vecs[i]
	}

	if err := s.store.Upsert(ctx, items); err != nil {
		return fmt.Errorf("upsert %d vectors; %w", len(items), err)
	}

	slog.Info("rag index updated", "repo", repo, "changed", len(changed), "removed", len(removed), "chunks", len(items))

	return nil
}

// DeleteRepo removes a repo's vectors from the index (on repo removal).
func (s *Service) DeleteRepo(ctx context.Context, repo string) error {
	return s.store.DeleteRepo(ctx, repo)
}

// HasRepo reports whether the docs vector index holds any chunk for the
// repo. The docs-index stage uses this to force a rebuild when the index
// is missing even though its stage state claims success (e.g. after the
// docs were regenerated by a run that did not re-embed them).
func (s *Service) HasRepo(ctx context.Context, repo string) (bool, error) {
	return s.store.HasRepo(ctx, repo)
}

// IndexedPaths returns the distinct doc paths that currently have vectors for
// the repo, so a caller can reconcile the on-disk docs against the index and
// re-embed any that are missing (e.g. after an interrupted embed run).
func (s *Service) IndexedPaths(ctx context.Context, repo string) (map[string]struct{}, error) {
	return s.store.IndexedPaths(ctx, repo)
}

// Retrieve returns up to topDocs bounded excerpts most relevant to the
// question. The filter selects which keys (repos, web-source collections or
// both) are searched; a zero filter searches everything. topDocs <= 0 uses
// the configured default (RAG.TopDocs).
func (s *Service) Retrieve(ctx context.Context, filter vectorstore.Filter, question string, topDocs int) ([]Doc, error) {
	if strings.TrimSpace(question) == "" {
		return nil, errors.New("question is empty")
	}

	if topDocs <= 0 {
		topDocs = s.cfg.TopDocs
	}

	if topDocs <= 0 {
		topDocs = DefaultTopDocs
	}
	if topDocs > MaxTopDocs {
		topDocs = MaxTopDocs
	}

	topK := s.cfg.TopK
	if topK <= 0 {
		topK = 20
	}

	// Fetch more chunks than docs wanted so grouping has material to rank.
	if topK < topDocs {
		topK = topDocs * 4
	}
	if topK > 40 {
		topK = 40
	}

	vecs, err := s.emb.Embed(ctx, []string{question})
	if err != nil {
		return nil, fmt.Errorf("embed question; %w", err)
	}

	if len(vecs) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors for the question", len(vecs))
	}

	matches, err := s.store.Search(ctx, filter, vecs[0], topK)
	if err != nil {
		return nil, fmt.Errorf("vector search; %w", err)
	}

	// Group chunk matches into documents; doc score = best chunk score.
	type docKey struct{ repo, path string }

	best := map[docKey]vectorstore.Match{}

	var order []docKey

	for _, m := range matches {
		k := docKey{m.Payload.Repo, m.Payload.DocPath}

		if prev, ok := best[k]; !ok {
			best[k] = m
			order = append(order, k)
		} else if m.Score > prev.Score {
			best[k] = m
		}
	}

	// Rank by a recency-adjusted score so a stale document does not outrank a
	// fresh, similarly-relevant one. The adjustment is mild (see rankScore) so
	// semantic relevance stays dominant; documents without a timestamp are
	// unaffected. Sorting uses the adjusted score; the returned Score is the
	// adjusted value and UpdatedAt is surfaced so the model can judge recency.
	now := time.Now()
	adj := func(k docKey) float32 { return rankScore(best[k].Score, best[k].Payload.UpdatedAt, now) }

	sort.SliceStable(order, func(i, j int) bool {
		return adj(order[i]) > adj(order[j])
	})

	docs := make([]Doc, 0, topDocs)

	for _, k := range order {
		if len(docs) == topDocs {
			break
		}

		m := best[k]

		excerpt, truncated := boundedExcerpt(m.Payload.Chunk)

		docs = append(docs, Doc{
			Repo:      k.repo,
			Path:      k.path,
			Title:     m.Payload.Title,
			Score:     adj(k),
			Excerpt:   excerpt,
			Truncated: truncated,
			UpdatedAt: m.Payload.UpdatedAt,
		})
	}

	return docs, nil
}

// recencyFloor caps how much an old document is penalised: its adjusted score
// never drops below this fraction of its semantic score. Kept high so recency
// only breaks near-ties and never buries a strongly-relevant old document.
const recencyFloor = 0.80

// recencyHalfLife is the age at which the recency penalty reaches half of its
// maximum. With a ~2-year half-life a 2-year-old doc loses ~half of the (1 -
// floor) budget, a decade-old doc approaches the floor, and anything within the
// last few months is essentially unpenalised.
const recencyHalfLife = 2 * 365 * 24 * time.Hour

// rankScore adjusts a semantic score by document age: fresh docs keep their
// full score, older docs are multiplied by a factor decaying from 1.0 toward
// recencyFloor with recencyHalfLife. A zero updatedAt (unknown) returns the
// score unchanged, so untimestamped sources are never penalised.
func rankScore(score float32, updatedAt, now time.Time) float32 {
	if updatedAt.IsZero() {
		return score
	}

	age := now.Sub(updatedAt)
	if age <= 0 {
		return score
	}

	// Exponential decay of the (1 - floor) budget: factor = floor + (1-floor)*2^(-age/halfLife).
	decay := math.Exp2(-float64(age) / float64(recencyHalfLife))
	factor := recencyFloor + (1-recencyFloor)*decay

	return score * float32(factor)
}

func boundedExcerpt(content string) (string, bool) {
	runes := []rune(content)
	if len(runes) <= MaxExcerptRunes {
		return content, false
	}

	return string(runes[:MaxExcerptRunes]), true
}

// manifestTitles maps doc path -> title from the docgen manifest, when present.
func manifestTitles(docsDir string) map[string]string {
	out := map[string]string{}

	man, err := docgen.LoadManifest(docsDir)
	if err != nil || man == nil {
		return out
	}

	for _, d := range man.Docs {
		out[d.Path] = d.Title
	}

	return out
}

// titlesIndexed counts distinct documents in an item batch (for logging).
func titlesIndexed(items []vectorstore.Item) map[string]struct{} {
	docs := map[string]struct{}{}

	for _, it := range items {
		docs[it.Payload.DocPath] = struct{}{}
	}

	return docs
}
