package rag

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"

	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/storage"
)

const (
	docsTextBucketName = "docs_search"
	docsTextBatchSize  = 100
	maxTextCandidates  = 40
)

type textRecord struct {
	ID        string    `bw:"id,pk"`
	Repo      string    `bw:"repo,index"`
	Path      string    `bw:"path,fts"`
	Title     string    `bw:"title,fts"`
	Excerpt   string    `bw:"excerpt,fts"`
	UpdatedAt time.Time `bw:"updated_at"`
}

// TextStore keeps the BM25 documentation index in Krabby's state database.
// It is independent of the embedder-backed vector index.
type TextStore struct {
	db     *bw.DB
	bucket *bw.Bucket[textRecord]

	// statsBucket holds the corpus-derived query tuning; see textstats.go.
	statsBucket *bw.Bucket[textStats]
	stats       statsCache

	// A full-text write expands into one key per distinct term, so how many
	// records fit in one Badger transaction depends on how long and how varied
	// the documents are. The batchers discover that instead of assuming it.
	writes  *storage.Batcher
	deletes *storage.Batcher
}

func NewTextStore(db *bw.DB) (*TextStore, error) {
	bucket, err := bw.RegisterBucket[textRecord](db, docsTextBucketName, bw.WithVersion[textRecord](1))
	if err != nil {
		return nil, fmt.Errorf("register docs search bucket; %w", err)
	}

	statsBucket, err := bw.RegisterBucket[textStats](db, docsStatsBucketName)
	if err != nil {
		return nil, fmt.Errorf("register docs search stats bucket; %w", err)
	}

	return &TextStore{
		db:          db,
		bucket:      bucket,
		statsBucket: statsBucket,
		writes:      storage.NewBatcher(docsTextBatchSize),
		deletes:     storage.NewBatcher(docsTextBatchSize),
	}, nil
}

// Index rebuilds one repository or web collection from its markdown files.
//
// The background warm pass runs this for every repository and every web
// collection at startup, so it streams: a synced JIRA project or Confluence
// space is thousands of documents, and holding all of their excerpts in memory
// made startup cost scale with the corpus.
func (s *TextStore) Index(ctx context.Context, repo, docsDir string) error {
	if err := s.DeleteRepo(ctx, repo); err != nil {
		return err
	}

	err := streamTextRecords(ctx, docsDir, repo, nil, nil, s.insert)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("docs dir %s does not exist; generate docs first", docsDir)
	}

	return err
}

// IndexPaths updates only changed markdown paths and removes stale paths.
func (s *TextStore) IndexPaths(ctx context.Context, repo, docsDir string, changed, removed []string, opts *IndexOptions) error {
	stale := make([]string, 0, len(changed)+len(removed))
	stale = append(stale, changed...)
	stale = append(stale, removed...)
	if err := s.DeletePaths(ctx, repo, stale); err != nil {
		return err
	}

	return streamTextRecords(ctx, docsDir, repo, changed, opts, s.insert)
}

// streamTextRecords reads the markdown under docsDir (all of it, or just paths
// when non-nil), turns it into search records and hands them to flush in
// batches of docsTextBatchSize. flush must not retain the slice.
func streamTextRecords(
	ctx context.Context,
	docsDir, repo string,
	paths []string,
	opts *IndexOptions,
	flush func(context.Context, []*textRecord) error,
) error {
	titles := manifestTitles(docsDir)
	batch := make([]*textRecord, 0, docsTextBatchSize)

	drain := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := flush(ctx, batch); err != nil {
			return err
		}
		// Clear before reslicing so the flushed excerpts become collectable
		// immediately rather than at the next overwrite.
		clear(batch)
		batch = batch[:0]

		return nil
	}

	add := func(docPath string) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		docPath = filepath.ToSlash(docPath)
		content, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(docPath)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && paths != nil {
				return nil
			}

			return err
		}

		title := titles[docPath]
		if title == "" {
			title = firstHeading(string(content))
		}
		if title == "" {
			title = docPath
		}

		var updatedAt time.Time
		if opts != nil && opts.UpdatedAt != nil {
			updatedAt = opts.UpdatedAt(docPath)
		}

		for i, excerpt := range chunk(string(content), 1200, 200) {
			batch = append(batch, &textRecord{
				ID:        fmt.Sprintf("%s/%s#%d", repo, docPath, i),
				Repo:      repo,
				Path:      docPath,
				Title:     title,
				Excerpt:   excerpt,
				UpdatedAt: updatedAt,
			})

			if len(batch) >= docsTextBatchSize {
				if err := drain(); err != nil {
					return err
				}
			}
		}

		return nil
	}

	if paths != nil {
		for _, docPath := range paths {
			if err := add(docPath); err != nil {
				return fmt.Errorf("read docs text path %s; %w", docPath, err)
			}
		}

		return drain()
	}

	err := filepath.WalkDir(docsDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}

		rel, err := filepath.Rel(docsDir, path)
		if err != nil {
			return err
		}

		return add(rel)
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return err
		}

		return fmt.Errorf("walk docs text dir; %w", err)
	}

	return drain()
}

func (s *TextStore) insert(ctx context.Context, records []*textRecord) error {
	if err := storage.Run(s.writes, records, func(batch []*textRecord) error {
		return s.bucket.InsertMany(ctx, batch)
	}); err != nil {
		return fmt.Errorf("insert docs search chunks; %w", err)
	}

	return nil
}

// Search performs BM25 search and returns the best matching chunk per document.
//
// The index is chunked, so several hits can belong to one document and the
// caller's document budget cannot be expressed as a record limit. The hits are
// therefore streamed in rank order and the walk stops at the first point where
// topDocs distinct documents have been collected.
//
// This used to page through the ranked hits in fixed batches, re-issuing the
// query for each page. bw evaluates the whole query per call and paginates by
// slicing the result, so on a large corpus (tens of thousands of documents) the
// scan cost was paid once per page: a filtered search that had to walk deep
// into the ranking degenerated into a quadratic scan and effectively hung. One
// streamed pass with the filter pushed down removes both the repeated
// evaluation and the hydration of records the filter rejects.
func (s *TextStore) Search(ctx context.Context, filter vectorstore.Filter, search string, topDocs int) ([]Doc, error) {
	if strings.TrimSpace(search) == "" {
		return nil, errors.New("question is empty")
	}
	if topDocs <= 0 {
		topDocs = DefaultTopDocs
	}
	if topDocs > maxTextCandidates {
		topDocs = maxTextCandidates
	}

	seen := make(map[string]struct{}, topDocs)
	docs := make([]Doc, 0, topDocs)

	_, err := s.bucket.SearchWalk(ctx, search, bw.SearchOptions{Filter: filter.Query()},
		func(hit bw.SearchResult[textRecord]) (bool, error) {
			rec := hit.Record
			if rec == nil {
				return true, nil
			}

			key := rec.Repo + "\x00" + rec.Path
			if _, ok := seen[key]; ok {
				return true, nil
			}
			seen[key] = struct{}{}

			excerpt, truncated := boundedExcerpt(rec.Excerpt)
			docs = append(docs, Doc{
				Repo:      rec.Repo,
				Path:      rec.Path,
				Title:     rec.Title,
				Score:     float32(hit.Score),
				Excerpt:   excerpt,
				Truncated: truncated,
				UpdatedAt: rec.UpdatedAt,
			})

			return len(docs) < topDocs, nil
		})
	if err != nil {
		return nil, fmt.Errorf("docs text search; %w", err)
	}

	return docs, nil
}

// HasRepo reports whether the key has any indexed chunk. It stops at the first
// one: the answer is a yes/no, and counting a large collection's chunks to
// produce it costs a scan of the whole partition.
func (s *TextStore) HasRepo(ctx context.Context, repo string) (bool, error) {
	return s.bucket.Exists(ctx, docsTextRepoQuery(repo))
}

func (s *TextStore) IndexedPaths(ctx context.Context, repo string) (map[string]struct{}, error) {
	paths := map[string]struct{}{}
	err := s.bucket.Walk(ctx, docsTextRepoQuery(repo), func(record *textRecord) error {
		paths[record.Path] = struct{}{}
		return nil
	})

	return paths, err
}

func (s *TextStore) DeleteRepo(ctx context.Context, repo string) error {
	return s.deleteWhere(ctx, docsTextRepoQuery(repo), nil)
}

func (s *TextStore) DeletePaths(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		set[filepath.ToSlash(path)] = struct{}{}
	}

	return s.deleteWhere(ctx, docsTextRepoQuery(repo), set)
}

func (s *TextStore) deleteWhere(ctx context.Context, q *query.Query, paths map[string]struct{}) error {
	var ids []string
	if err := s.bucket.Walk(ctx, q, func(record *textRecord) error {
		if paths == nil {
			ids = append(ids, record.ID)
		} else if _, ok := paths[record.Path]; ok {
			ids = append(ids, record.ID)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("collect docs search chunks; %w", err)
	}

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
		return fmt.Errorf("delete docs search chunks; %w", err)
	}

	return nil
}

func docsTextRepoQuery(repo string) *query.Query {
	q := query.New()
	q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "repo", repo).Expression())
	return q
}
