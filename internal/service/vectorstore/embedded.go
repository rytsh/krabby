package vectorstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"
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
	Symbol    string    `bw:"symbol"`
	StartLine int       `bw:"start_line"`
	EndLine   int       `bw:"end_line"`
	Vector    []float32 `bw:"vector,vector(metric=cosine)"`
}

// bucketName is the bw bucket holding all chunks (all repos).
const bucketName = "chunks"

// deleteBatch bounds how many records are deleted per Badger transaction so
// large repos do not hit ErrTxnTooBig.
const deleteBatch = 500

// sharedHandle is a refcounted bw DB. Manager.Configure builds the new bundle
// (opening the store) before closing the previous one; Badger's directory lock
// forbids two concurrent opens, so both bundles share one handle and the DB
// closes only when the last reference is released.
type sharedHandle struct {
	dir    string
	db     *bw.DB
	bucket *bw.Bucket[chunkRecord]
	refs   int

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

	db, err := bw.Open(dir, bw.WithLogger(nil))
	if err != nil {
		return nil, fmt.Errorf("open vector db %s; %w", dir, err)
	}

	bucket, err := bw.RegisterBucket[chunkRecord](db, bucketName)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("register vector bucket; %w", err)
	}

	h := &sharedHandle{dir: dir, db: db, bucket: bucket, refs: 1}
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
			Symbol:    it.Payload.Symbol,
			StartLine: it.Payload.StartLine,
			EndLine:   it.Payload.EndLine,
			Vector:    it.Vector,
		})
	}

	s.h.opMu.RLock()
	err := s.h.bucket.InsertMany(ctx, records)
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
	err = s.h.bucket.InsertMany(ctx, records)
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

	return s.h.bucket.InsertMany(ctx, records)
}

func (s *embedded) Search(ctx context.Context, repo string, vec []float32, topK int) ([]Match, error) {
	if topK <= 0 {
		return nil, nil
	}

	opts := bw.SearchVectorOptions{K: topK}
	if repo != "" {
		opts.Filter = repoQuery(repo)
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
				Symbol:    h.Record.Symbol,
				StartLine: h.Record.StartLine,
				EndLine:   h.Record.EndLine,
			},
		})
	}

	return matches, nil
}

func (s *embedded) DeleteRepo(ctx context.Context, repo string) error {
	s.h.opMu.RLock()
	defer s.h.opMu.RUnlock()

	var ids []string

	err := s.h.bucket.Walk(ctx, repoQuery(repo), func(r *chunkRecord) error {
		ids = append(ids, r.ID)

		return nil
	})
	if err != nil {
		return fmt.Errorf("collect repo vectors; %w", err)
	}

	for start := 0; start < len(ids); start += deleteBatch {
		end := min(start+deleteBatch, len(ids))

		err := s.h.db.Update(func(tx *bw.Tx) error {
			for _, id := range ids[start:end] {
				if err := s.h.bucket.DeleteTx(tx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
					return err
				}
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("delete repo vectors; %w", err)
		}
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
