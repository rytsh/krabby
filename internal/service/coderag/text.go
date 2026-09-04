package coderag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"

	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/storage"
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
	// Snippet carries two indexes over the same bytes: BM25 for term
	// queries and a byte-trigram index for regular-expression search.
	Snippet string `bw:"snippet,fts,trigram"`
}

// RepoIndex reports the state of one repository's code index at the moment a
// search read it.
//
// A hit is only ever as current as the index it came from, and nothing else in
// the result says so: an agent that reads a chunk indexed three commits ago
// has no way to tell it apart from one indexed a second ago. Stale is the
// actionable bit — the clone has moved past what was indexed, so a refresh is
// pending or was skipped.
type RepoIndex struct {
	Repo      string    `json:"repo"`
	Commit    string    `json:"commit,omitempty"`
	IndexedAt time.Time `json:"indexed_at,omitzero"`
	Stale     bool      `json:"stale,omitempty"`
}

// SearchPage is one page of exact full-text code-search results.
type SearchPage struct {
	Results []Snippet `json:"results"`
	Total   uint64    `json:"total"`
	Page    int       `json:"page"`
	PerPage int       `json:"per_page"`
	// Indexed reports the index state of the repositories behind this page.
	Indexed []RepoIndex `json:"indexed,omitempty"`
}

// TextSearchOptions configures a full-text code search.
type TextSearchOptions struct {
	Page    int
	PerPage int
	// Path is an optional glob over the repo-relative source path, applied by
	// the caller through Scope.
	Path string
	// KeyFilter, when non-nil, rejects a hit by its chunk id before the record
	// is read. It carries both the repository scope and any path filter, which
	// are one thing at this level: the chunk id is "<repo>/<path>#<n>", so both
	// are prefix or glob tests on a string the ranking already produced.
	KeyFilter func(id string) bool
}

// TextStore keeps the normal code-search index in Krabby's state database.
// FTS writes are committed atomically with their chunk records by bw.
type TextStore struct {
	db     *bw.DB
	bucket *bw.Bucket[textRecord]

	// A full-text write expands into one key per distinct term, and source
	// chunks are three times the size of documentation ones, so how many
	// records fit in one Badger transaction is discovered rather than assumed.
	writes  *storage.Batcher
	deletes *storage.Batcher

	// statsBucket holds the corpus-derived frequent-term set; stats memoises
	// it because search reads it on every query.
	statsBucket *bw.Bucket[codeStats]
	stats       statsCache
}

func NewTextStore(db *bw.DB) (*TextStore, error) {
	bucket, err := bw.RegisterBucket[textRecord](db, textBucketName,
		// v2 added the trigram index on Snippet. The stored shape is
		// unchanged, so the step is an identity rewrite: bw routes a
		// migration through the ordinary write path, and that is what
		// emits the trigram postings a chunk indexed under v1 never had.
		// Nothing is re-chunked and no clone is read.
		bw.WithTypedMigration[textRecord, textRecord](1, 2,
			func(_ context.Context, old *textRecord) (*textRecord, error) { return old, nil },
		),
	)
	if err != nil {
		return nil, fmt.Errorf("register code search bucket; %w", err)
	}

	statsBucket, err := registerStatsBucket(db)
	if err != nil {
		return nil, err
	}

	return &TextStore{
		db:          db,
		bucket:      bucket,
		writes:      storage.NewBatcher(textBatchSize),
		deletes:     storage.NewBatcher(textBatchSize),
		statsBucket: statsBucket,
	}, nil
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

	if err := storage.Run(s.writes, records, func(batch []*textRecord) error {
		return s.bucket.InsertMany(ctx, batch)
	}); err != nil {
		return fmt.Errorf("insert code search chunks; %w", err)
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
// The scope filter is applied before pagination so Total stays exact.
//
// A filtered search used to be served by asking bw for every ranked hit at
// once, building a second slice of the ones that matched, and taking twenty
// rows out of it. Nothing leaked — both slices were garbage by the time the
// caller saw the result — but while the call ran the live heap grew with the
// corpus, and a few concurrent searches on a common term could take a
// container down.
//
// Paging is pushed all the way down instead: bw hydrates only the page and
// reports how many hits matched without decoding the ones outside it. The
// total therefore stays exact, which a paged search needs — a count that
// stopped at some ceiling would make the pager lie about how deep the results
// go — while the cost of a search follows the page, not the index.
//
// The filter is on the key rather than on indexed fields because everything it
// needs is in the chunk id: the repository is its first segment and the path
// its middle (see the id built in coderag.streamItems). Judging a hit by its
// key costs a string compare; judging it by its fields costs a point read and
// a partial decode, per hit, which is the difference between an exact total
// being free and costing the whole repository.
func (s *TextStore) Search(ctx context.Context, search string, opts TextSearchOptions) (SearchPage, error) {
	page, perPage := opts.Page, opts.PerPage
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

	offset := math.MaxInt
	if offset64 := uint64(page-1) * uint64(perPage); offset64 <= math.MaxInt {
		offset = int(offset64)
	}

	var (
		matched uint64
		window  = make([]bw.SearchResult[textRecord], 0, perPage)
	)

	if _, err := s.bucket.SearchWalk(ctx, search,
		bw.SearchOptions{
			KeyFilter: opts.KeyFilter,
			Offset:    offset,
			Limit:     perPage,
			Matched:   &matched,
		},
		func(hit bw.SearchResult[textRecord]) (bool, error) {
			if hit.Record != nil {
				window = append(window, hit)
			}

			return true, nil
		},
	); err != nil {
		return result, err
	}

	result.Total = matched
	result.Results = textSnippets(window, search)

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

// HasRepo reports whether the repo has any indexed chunk, stopping at the
// first one rather than counting them all.
func (s *TextStore) HasRepo(ctx context.Context, repo string) (bool, error) {
	return s.bucket.Exists(ctx, textRepoQuery(repo))
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
	if err := storage.Run(s.deletes, ids, func(batch []string) error {
		return s.db.Update(func(tx *bw.Tx) error {
			for _, id := range batch {
				if err := s.bucket.DeleteTx(tx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
					return err
				}
			}

			return nil
		})
	}); err != nil {
		return fmt.Errorf("delete code search chunks; %w", err)
	}

	return nil
}

func textRepoQuery(repo string) *query.Query {
	q := query.New()
	q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "repo", repo).Expression())
	return q
}
