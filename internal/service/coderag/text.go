package coderag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

const (
	textBucketName = "code_search"
	textBatchSize  = 100
	defaultPerPage = 20
	maxPerPage     = 100
)

type textRecord struct {
	ID        string `bw:"id,pk"`
	Repo      string `bw:"repo,index"`
	Path      string `bw:"path,fts"`
	Symbol    string `bw:"symbol,fts"`
	StartLine int    `bw:"start_line"`
	EndLine   int    `bw:"end_line"`
	Snippet   string `bw:"snippet,fts"`
}

// SearchPage is one page of exact full-text code-search results.
type SearchPage struct {
	Results []Snippet `json:"results"`
	Total   uint64    `json:"total"`
	Page    int       `json:"page"`
	PerPage int       `json:"per_page"`
}

// TextStore keeps the normal code-search index in Krabby's state database.
// FTS writes are committed atomically with their chunk records by bw.
type TextStore struct {
	db     *bw.DB
	bucket *bw.Bucket[textRecord]
}

func NewTextStore(db *bw.DB) (*TextStore, error) {
	bucket, err := bw.RegisterBucket[textRecord](db, textBucketName, bw.WithVersion[textRecord](1))
	if err != nil {
		return nil, fmt.Errorf("register code search bucket; %w", err)
	}

	return &TextStore{db: db, bucket: bucket}, nil
}

// ReplaceRepo replaces all searchable chunks for a repository.
func (s *TextStore) ReplaceRepo(ctx context.Context, repo string, items []vectorstore.Item) error {
	if err := s.DeleteRepo(ctx, repo); err != nil {
		return err
	}

	return s.InsertItems(ctx, items)
}

// InsertItems adds (or overwrites by ID) searchable chunks.
func (s *TextStore) InsertItems(ctx context.Context, items []vectorstore.Item) error {
	records := make([]*textRecord, 0, len(items))
	for _, item := range items {
		records = append(records, &textRecord{
			ID:        item.ID,
			Repo:      item.Payload.Repo,
			Path:      item.Payload.DocPath,
			Symbol:    item.Payload.Symbol,
			StartLine: item.Payload.StartLine,
			EndLine:   item.Payload.EndLine,
			Snippet:   item.Payload.Chunk,
		})
	}

	for start := 0; start < len(records); start += textBatchSize {
		end := min(start+textBatchSize, len(records))
		if err := s.bucket.InsertMany(ctx, records[start:end]); err != nil {
			return fmt.Errorf("insert code search chunks; %w", err)
		}
	}

	return nil
}

// DeletePaths removes a repo's chunks whose source path is in paths. Used for
// incremental re-indexing of changed/deleted files.
func (s *TextStore) DeletePaths(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}

	var ids []string
	if err := s.bucket.Walk(ctx, textRepoQuery(repo), func(record *textRecord) error {
		if _, ok := set[record.Path]; ok {
			ids = append(ids, record.ID)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("collect code search chunks; %w", err)
	}

	return s.deleteIDs(ids)
}

// Search performs BM25 full-text search over paths, symbols and source chunks.
// A repository filter is applied before pagination so Total stays exact.
func (s *TextStore) Search(ctx context.Context, repo, search string, page, perPage int) (SearchPage, error) {
	if page < 1 {
		page = 1
	}
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}

	result := SearchPage{Results: []Snippet{}, Page: page, PerPage: perPage}
	offset64 := uint64(page-1) * uint64(perPage)

	if repo == "" {
		offset := math.MaxInt
		if offset64 <= math.MaxInt {
			offset = int(offset64)
		}
		hits, total, err := s.bucket.Search(ctx, search, perPage, offset)
		if err != nil {
			return result, err
		}

		result.Total = total
		result.Results = textSnippets(hits, search)

		return result, nil
	}

	// bw FTS currently has no structured-filter option. Hydrate the ranked hit
	// set once, then filter by the indexed repo field before slicing the page.
	hits, _, err := s.bucket.Search(ctx, search, math.MaxInt, 0)
	if err != nil {
		return result, err
	}

	filtered := make([]bw.SearchResult[textRecord], 0, len(hits))
	for _, hit := range hits {
		if hit.Record != nil && hit.Record.Repo == repo {
			filtered = append(filtered, hit)
		}
	}
	result.Total = uint64(len(filtered))
	if offset64 >= result.Total {
		return result, nil
	}

	start := int(offset64)
	end := min(start+perPage, len(filtered))
	result.Results = textSnippets(filtered[start:end], search)

	return result, nil
}

func textSnippets(hits []bw.SearchResult[textRecord], search string) []Snippet {
	out := make([]Snippet, 0, len(hits))
	for _, hit := range hits {
		if hit.Record == nil {
			continue
		}
		r := hit.Record
		out = append(out, Snippet{
			Repo:      r.Repo,
			Path:      r.Path,
			Symbol:    r.Symbol,
			StartLine: r.StartLine,
			EndLine:   r.EndLine,
			Line:      matchingLine(r, search),
			Score:     float32(hit.Score),
			Snippet:   r.Snippet,
		})
	}

	return out
}

// matchingLine finds the source line containing the most query terms. bw FTS
// can also match a path or symbol, so the chunk start remains the safe fallback.
func matchingLine(record *textRecord, search string) int {
	tokenizer := bw.DefaultTokenizer{MinLen: 1}
	queryTerms := tokenizer.Tokenize(search)
	if len(queryTerms) == 0 {
		return record.StartLine
	}

	querySet := make(map[string]struct{}, len(queryTerms))
	for _, term := range queryTerms {
		querySet[term] = struct{}{}
	}

	bestLine, bestScore := record.StartLine, 0
	for i, line := range strings.Split(record.Snippet, "\n") {
		lineTerms := tokenizer.Tokenize(line)
		seen := make(map[string]struct{}, len(lineTerms))
		score := 0
		for _, term := range lineTerms {
			if _, wanted := querySet[term]; !wanted {
				continue
			}
			if _, counted := seen[term]; counted {
				continue
			}
			seen[term] = struct{}{}
			score++
		}

		if score > bestScore {
			bestLine = record.StartLine + i
			bestScore = score
		}
	}

	return bestLine
}

func (s *TextStore) HasRepo(ctx context.Context, repo string) (bool, error) {
	n, err := s.bucket.Count(ctx, textRepoQuery(repo))
	return n > 0, err
}

func (s *TextStore) DeleteRepo(ctx context.Context, repo string) error {
	var ids []string
	if err := s.bucket.Walk(ctx, textRepoQuery(repo), func(record *textRecord) error {
		ids = append(ids, record.ID)
		return nil
	}); err != nil {
		return fmt.Errorf("collect code search chunks; %w", err)
	}

	return s.deleteIDs(ids)
}

func (s *TextStore) deleteIDs(ids []string) error {
	for start := 0; start < len(ids); start += textBatchSize {
		end := min(start+textBatchSize, len(ids))
		if err := s.db.Update(func(tx *bw.Tx) error {
			for _, id := range ids[start:end] {
				if err := s.bucket.DeleteTx(tx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
					return err
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("delete code search chunks; %w", err)
		}
	}

	return nil
}

func textRepoQuery(repo string) *query.Query {
	q := query.New()
	q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "repo", repo).Expression())
	return q
}
