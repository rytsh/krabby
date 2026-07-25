package vectorstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"

	"github.com/rytsh/krabby/internal/memlimit"
	"github.com/rytsh/krabby/internal/storage"
)

// embedded is the default vector store, backed by a bw (BadgerDB) database
// under its configured data directory. Vectors live in an HNSW index (cosine), payloads in the
// same record, so search + payload fetch is one lookup. It keeps krabby's
// zero-infra promise: everything is plain files under data_dir.
//
// The embedding dimension is auto-detected by bw on first insert and locked in
// the bucket manifest. When the embedding model (and so the dimension) changes,
// Upsert wipes the derived index and retries once: vectors are always
// rebuildable from the markdown docs.
type embedded struct {
	h *sharedHandle

	// wipeMu serialises the dim-mismatch wipe+retry path.
	wipeMu sync.Mutex
}

// chunkRecord is one embedded chunk in the bw bucket.
type chunkRecord struct {
	ID        string    `bw:"id,pk"`
	Repo      string    `bw:"repo,index"`
	DocPath   string    `bw:"doc_path"`
	Title     string    `bw:"title"`
	Chunk     string    `bw:"chunk"`
	UpdatedAt time.Time `bw:"updated_at"`
	Symbol    string    `bw:"symbol"`
	StartLine int       `bw:"start_line"`
	EndLine   int       `bw:"end_line"`
	Vector    []float32 `bw:"vector,vector(metric=cosine)"`
}

// bucketName is the bw bucket holding all chunks (all repos).
const bucketName = "chunks"

// bucketVersion is bumped whenever chunkRecord changes shape so bw performs a
// migration instead of refusing to open on a schema-fingerprint mismatch. The
// vector index is derived data (rebuilt from the docs on disk), so a version
// bump that drops incompatible rows is acceptable; sources re-embed on their
// next sync.
//   - v2: added UpdatedAt for recency-aware retrieval.
const bucketVersion = 2

// deleteBatch and upsertBatch are the starting points for the adaptive
// batchers below, not hard limits. Each vector record carries a full embedding
// whose width is set by the model, so how many fit in one Badger transaction
// cannot be known ahead of time; storage.Run discovers it.
const (
	deleteBatch = 500
	upsertBatch = 64
)

// sharedHandle is a refcounted bw DB. Manager.Configure builds the new bundle
// (opening the store) before closing the previous one; Badger's directory lock
// forbids two concurrent opens, so both bundles share one handle and the DB
// closes only when the last reference is released.
type sharedHandle struct {
	dir    string
	db     *bw.DB
	bucket *bw.Bucket[chunkRecord]
	refs   int

	// Batch sizes are per database, not per handle: they describe how wide
	// this store's records are, which every handle sharing it observes.
	upserts *storage.Batcher
	deletes *storage.Batcher

	// opMu lets ordinary operations run concurrently but makes a dimension
	// migration (Wipe + first insert) exclusive across every handle sharing the
	// same database.
	opMu sync.RWMutex
}

var sharedDBs = struct {
	sync.Mutex
	m map[string]*sharedHandle
}{m: map[string]*sharedHandle{}}

func newEmbedded(dir string) (*embedded, error) {
	sharedDBs.Lock()
	defer sharedDBs.Unlock()

	if h, ok := sharedDBs.m[dir]; ok {
		h.refs++

		return &embedded{h: h}, nil
	}

	// Vector databases are the largest of krabby's three Badger stores, so the
	// shared tuning (bounded caches, small memtables) matters most here.
	db, err := storage.OpenTuned(dir, memlimit.Current())
	if err != nil {
		return nil, fmt.Errorf("open vector db %s; %w", dir, err)
	}

	bucket, err := bw.RegisterBucket[chunkRecord](db, bucketName, bw.WithVersion[chunkRecord](bucketVersion))
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("register vector bucket; %w", err)
	}

	h := &sharedHandle{
		dir: dir, db: db, bucket: bucket, refs: 1,
		upserts: storage.NewBatcher(upsertBatch),
		deletes: storage.NewBatcher(deleteBatch),
	}
	sharedDBs.m[dir] = h

	return &embedded{h: h}, nil
}

func (s *embedded) Upsert(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return nil
	}

	records := make([]*chunkRecord, 0, len(items))
	for _, it := range items {
		records = append(records, &chunkRecord{
			ID:        it.ID,
			Repo:      it.Payload.Repo,
			DocPath:   it.Payload.DocPath,
			Title:     it.Payload.Title,
			Chunk:     it.Payload.Chunk,
			UpdatedAt: it.Payload.UpdatedAt,
			Symbol:    it.Payload.Symbol,
			StartLine: it.Payload.StartLine,
			EndLine:   it.Payload.EndLine,
			Vector:    it.Vector,
		})
	}

	s.h.opMu.RLock()
	err := s.insertBatched(ctx, records)
	s.h.opMu.RUnlock()
	if err == nil {
		return nil
	}

	if !errors.Is(err, bw.ErrDimMismatch) {
		return err
	}

	// The embedding dimension changed (new model). The index is derived data,
	// so wipe it and retry once; other repos re-index on their next refresh.
	s.wipeMu.Lock()
	defer s.wipeMu.Unlock()
	s.h.opMu.Lock()
	defer s.h.opMu.Unlock()

	// Another concurrent upsert may have completed the migration while this
	// call waited. Recheck before wiping so completed repo indexes are not lost.
	err = s.insertBatched(ctx, records)
	if err == nil {
		return nil
	}

	if !errors.Is(err, bw.ErrDimMismatch) {
		return err
	}

	slog.Warn("embedding dimension changed; wiping vector index for rebuild",
		"dir", s.h.dir, "error", err)

	if werr := s.h.db.Wipe(); werr != nil {
		return fmt.Errorf("wipe vector db after dim change; %w", werr)
	}

	return s.insertBatched(ctx, records)
}

// insertBatched inserts records in batches sized to fit Badger's
// per-transaction limit. On a dimension mismatch it returns immediately with
// that error so the caller's wipe+retry path can run.
func (s *embedded) insertBatched(ctx context.Context, records []*chunkRecord) error {
	return storage.Run(s.h.upserts, records, func(batch []*chunkRecord) error {
		return s.h.bucket.InsertMany(ctx, batch)
	})
}

func (s *embedded) Search(ctx context.Context, filter Filter, vec []float32, topK int) ([]Match, error) {
	if topK <= 0 {
		return nil, nil
	}

	opts := bw.SearchVectorOptions{K: topK}
	if q := filterQuery(filter); q != nil {
		opts.Filter = q
	}

	s.h.opMu.RLock()
	defer s.h.opMu.RUnlock()

	hits, err := s.h.bucket.SearchVector(ctx, vec, opts)
	if err != nil {
		if errors.Is(err, bw.ErrDimMismatch) {
			// Model changed but nothing re-indexed yet under the new dimension.
			return nil, fmt.Errorf("query dimension does not match the index; re-index docs first; %w", err)
		}

		return nil, err
	}

	matches := make([]Match, 0, len(hits))
	for _, h := range hits {
		matches = append(matches, Match{
			Score: float32(h.Score),
			Payload: Payload{
				Repo:      h.Record.Repo,
				DocPath:   h.Record.DocPath,
				Title:     h.Record.Title,
				Chunk:     h.Record.Chunk,
				UpdatedAt: h.Record.UpdatedAt,
				Symbol:    h.Record.Symbol,
				StartLine: h.Record.StartLine,
				EndLine:   h.Record.EndLine,
			},
		})
	}

	return matches, nil
}

func (s *embedded) DeleteRepo(ctx context.Context, repo string) error {
	return s.deleteWhere(ctx, repo, nil)
}

// HasRepo reports whether any vector exists for the repo. It stops at the
// first matching record via a sentinel error so it does not scan the
// whole repo partition.
func (s *embedded) HasRepo(ctx context.Context, repo string) (bool, error) {
	s.h.opMu.RLock()
	defer s.h.opMu.RUnlock()

	found := false
	err := s.h.bucket.Walk(ctx, repoQuery(repo), func(_ *chunkRecord) error {
		found = true

		return errStopWalk
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return false, fmt.Errorf("scan repo vectors; %w", err)
	}

	return found, nil
}

// errStopWalk short-circuits a bw.Walk once the first record is seen.
var errStopWalk = errors.New("stop walk")

// IndexedPaths returns the distinct DocPaths that have at least one vector for
// the repo, by scanning the repo's chunk records.
func (s *embedded) IndexedPaths(ctx context.Context, repo string) (map[string]struct{}, error) {
	s.h.opMu.RLock()
	defer s.h.opMu.RUnlock()

	paths := map[string]struct{}{}
	err := s.h.bucket.Walk(ctx, repoQuery(repo), func(rec *chunkRecord) error {
		if rec.DocPath != "" {
			paths[rec.DocPath] = struct{}{}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan repo vectors; %w", err)
	}

	return paths, nil
}

func (s *embedded) DeletePaths(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}

	return s.deleteWhere(ctx, repo, set)
}

// deleteWhere removes a repo's records, optionally restricted to a DocPath set
// (nil = all records of the repo).
func (s *embedded) deleteWhere(ctx context.Context, repo string, paths map[string]struct{}) error {
	s.h.opMu.RLock()
	defer s.h.opMu.RUnlock()

	var ids []string

	err := s.h.bucket.Walk(ctx, repoQuery(repo), func(r *chunkRecord) error {
		if paths != nil {
			if _, ok := paths[r.DocPath]; !ok {
				return nil
			}
		}

		ids = append(ids, r.ID)

		return nil
	})
	if err != nil {
		return fmt.Errorf("collect repo vectors; %w", err)
	}

	if err := storage.Run(s.h.deletes, ids, func(batch []string) error {
		return s.h.db.Update(func(tx *bw.Tx) error {
			for _, id := range batch {
				if err := s.h.bucket.DeleteTx(tx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
					return err
				}
			}

			return nil
		})
	}); err != nil {
		return fmt.Errorf("delete repo vectors; %w", err)
	}

	return nil
}

func (s *embedded) Close() error {
	sharedDBs.Lock()
	defer sharedDBs.Unlock()

	h := s.h
	if h == nil {
		return nil
	}

	s.h = nil

	h.refs--
	if h.refs > 0 {
		return nil
	}

	delete(sharedDBs.m, h.dir)

	return h.db.Close()
}

// repoQuery builds the bw query filter matching one repo.
func repoQuery(repo string) *query.Query {
	q := query.New()
	q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "repo", repo).Expression())

	return q
}

// filterQuery translates a search Filter into a bw where clause, or nil when
// the filter matches everything.
func filterQuery(f Filter) *query.Query { return f.Query() }
