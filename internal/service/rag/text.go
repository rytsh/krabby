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
)

const (
	docsTextBucketName = "docs_search"
	docsTextBatchSize  = 100
	docsSearchBatch    = 200
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

	return &TextStore{db: db, bucket: bucket, statsBucket: statsBucket}, nil
}

// Index rebuilds one repository or web collection from its markdown files.
func (s *TextStore) Index(ctx context.Context, repo, docsDir string) error {
	records, err := textRecords(docsDir, nil, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("docs dir %s does not exist; generate docs first", docsDir)
		}

		return err
	}
	for _, record := range records {
		record.Repo = repo
		record.ID = repo + "/" + record.ID
	}

	if err := s.DeleteRepo(ctx, repo); err != nil {
		return err
	}

	return s.insert(ctx, records)
}

// IndexPaths updates only changed markdown paths and removes stale paths.
func (s *TextStore) IndexPaths(ctx context.Context, repo, docsDir string, changed, removed []string, opts *IndexOptions) error {
	stale := make([]string, 0, len(changed)+len(removed))
	stale = append(stale, changed...)
	stale = append(stale, removed...)
	if err := s.DeletePaths(ctx, repo, stale); err != nil {
		return err
	}

	records, err := textRecords(docsDir, changed, opts)
	if err != nil {
		return err
	}
	for _, record := range records {
		record.Repo = repo
		record.ID = repo + "/" + record.ID
	}

	return s.insert(ctx, records)
}

func textRecords(docsDir string, paths []string, opts *IndexOptions) ([]*textRecord, error) {
	titles := manifestTitles(docsDir)
	var records []*textRecord

	add := func(docPath string) error {
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
			records = append(records, &textRecord{
				ID:        fmt.Sprintf("%s#%d", docPath, i),
				Path:      docPath,
				Title:     title,
				Excerpt:   excerpt,
				UpdatedAt: updatedAt,
			})
		}

		return nil
	}

	if paths != nil {
		for _, docPath := range paths {
			if err := add(docPath); err != nil {
				return nil, fmt.Errorf("read docs text path %s; %w", docPath, err)
			}
		}

		return records, nil
	}

	err := filepath.WalkDir(docsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
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
		return nil, fmt.Errorf("walk docs text dir; %w", err)
	}

	return records, nil
}

func (s *TextStore) insert(ctx context.Context, records []*textRecord) error {
	for start := 0; start < len(records); start += docsTextBatchSize {
		end := min(start+docsTextBatchSize, len(records))
		if err := s.bucket.InsertMany(ctx, records[start:end]); err != nil {
			return fmt.Errorf("insert docs search chunks; %w", err)
		}
	}

	return nil
}

// Search performs BM25 search and returns the best matching chunk per document.
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

	// bw FTS has no structured-filter option. Walk ranked hits in bounded pages
	// until enough documents survive the repo/web-source filter.
	for offset := 0; ; offset += docsSearchBatch {
		hits, total, err := s.bucket.Search(ctx, search, docsSearchBatch, offset)
		if err != nil {
			return nil, fmt.Errorf("docs text search; %w", err)
		}
		for _, hit := range hits {
			if hit.Record == nil || !textFilterMatches(filter, hit.Record.Repo) {
				continue
			}
			key := hit.Record.Repo + "\x00" + hit.Record.Path
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			excerpt, truncated := boundedExcerpt(hit.Record.Excerpt)
			docs = append(docs, Doc{
				Repo:      hit.Record.Repo,
				Path:      hit.Record.Path,
				Title:     hit.Record.Title,
				Score:     float32(hit.Score),
				Excerpt:   excerpt,
				Truncated: truncated,
				UpdatedAt: hit.Record.UpdatedAt,
			})
			if len(docs) == topDocs {
				return docs, nil
			}
		}
		if uint64(offset+docsSearchBatch) >= total {
			break
		}
	}

	return docs, nil
}

func textFilterMatches(filter vectorstore.Filter, repo string) bool {
	if len(filter.Keys) > 0 {
		matched := false
		for _, key := range filter.Keys {
			if repo == key {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if filter.Prefix != "" && !strings.HasPrefix(repo, filter.Prefix) {
		return false
	}
	if filter.ExcludePrefix != "" && strings.HasPrefix(repo, filter.ExcludePrefix) {
		return false
	}

	return true
}

func (s *TextStore) HasRepo(ctx context.Context, repo string) (bool, error) {
	n, err := s.bucket.Count(ctx, docsTextRepoQuery(repo))
	return n > 0, err
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

	for start := 0; start < len(ids); start += docsTextBatchSize {
		end := min(start+docsTextBatchSize, len(ids))
		if err := s.db.Update(func(tx *bw.Tx) error {
			for _, id := range ids[start:end] {
				if err := s.bucket.DeleteTx(tx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
					return err
				}
			}

			return nil
		}); err != nil {
			return fmt.Errorf("delete docs search chunks; %w", err)
		}
	}

	return nil
}

func docsTextRepoQuery(repo string) *query.Query {
	q := query.New()
	q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "repo", repo).Expression())
	return q
}
