// Package taskstore persists the background work queue so queued (and
// interrupted running) tasks survive a service restart.
//
// The queue package holds tasks in memory; before this store a restart dropped
// the whole backlog and any half-finished job silently disappeared. This store
// records a serializable description of each task (its queue.Spec) keyed by the
// task's sequence number, and removes the record once the task reaches a
// terminal state. On startup the manager reads the surviving records and
// re-enqueues them, so nothing is lost across restarts.
package taskstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"

	"github.com/rytsh/krabby/internal/service/queue"
)

// Record is one persisted queue task. Params is the JSON-encoded queue.Spec
// params map, kept as a string so the store never depends on the encoder's map
// support. Seq is the queue sequence number and the primary key (as a string so
// it round-trips through bw's string keys).
type Record struct {
	Seq        string    `bw:"seq,pk"       json:"seq"`
	Kind       string    `bw:"kind"         json:"kind"`
	ID         string    `bw:"id"           json:"id"`
	Params     string    `bw:"params"       json:"params,omitempty"`
	EnqueuedAt time.Time `bw:"enqueued_at"  json:"enqueued_at,omitzero"`
}

// schemaVersion is bumped whenever Record changes shape so bw auto-migrates the
// bucket instead of failing on a fingerprint mismatch.
const schemaVersion = 1

// opTimeout bounds a single read/write against the local embedded store; it is
// generous because a persistence stall must never block the queue itself.
const opTimeout = 5 * time.Second

// Store persists queue tasks in a bw bucket. It implements queue.Persister.
type Store struct {
	bucket *bw.Bucket[Record]
}

// New opens the tasks bucket on the given database.
func New(db *bw.DB) (*Store, error) {
	bucket, err := bw.RegisterBucket[Record](db, "tasks", bw.WithVersion[Record](schemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register tasks bucket; %w", err)
	}

	return &Store{bucket: bucket}, nil
}

// Save records (or replaces) a queued task. It is called by the queue when a
// task is enqueued. Errors are logged-and-swallowed: a persistence failure must
// never block or fail the in-memory enqueue.
func (s *Store) Save(seq uint64, spec queue.Spec, enqueuedAt time.Time) {
	params := marshalParams(spec.Params)

	rec := &Record{
		Seq:        strconv.FormatUint(seq, 10),
		Kind:       spec.Kind,
		ID:         spec.ID,
		Params:     params,
		EnqueuedAt: enqueuedAt,
	}

	// Insert acts as an upsert in bw; a background timeout would be unusual for
	// a local embedded store, so a short bounded context is enough.
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	if err := s.bucket.Insert(ctx, rec); err != nil {
		// The queue interface logs nothing itself; surface it here.
		slog.Error("persist queued task", "seq", seq, "error", err)
	}
}

// marshalParams encodes a spec's params map to a JSON string for storage,
// returning "" for an empty map or an encoding error (the params are advisory).
func marshalParams(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}

	b, err := json.Marshal(params)
	if err != nil {
		return ""
	}

	return string(b)
}

// Remove drops a task's record once it reaches a terminal state. It is a no-op
// when the record is already gone.
func (s *Store) Remove(seq uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	if err := s.bucket.Delete(ctx, strconv.FormatUint(seq, 10)); err != nil && !errors.Is(err, bw.ErrNotFound) {
		slog.Error("remove persisted task", "seq", seq, "error", err)
	}
}

// PersistedTask is one restored task: its original queue seq plus the decoded
// spec needed to rebuild the Run closure.
type PersistedTask struct {
	Seq        uint64
	Spec       queue.Spec
	EnqueuedAt time.Time
}

// List returns every persisted task ordered by seq (ascending), so the manager
// re-enqueues them in their original FIFO order after a restart.
func (s *Store) List(ctx context.Context) ([]PersistedTask, error) {
	q, err := query.Parse("_limit=100000")
	if err != nil {
		return nil, fmt.Errorf("parse query; %w", err)
	}

	recs, err := s.bucket.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list tasks; %w", err)
	}

	out := make([]PersistedTask, 0, len(recs))
	for _, rec := range recs {
		seq, perr := strconv.ParseUint(rec.Seq, 10, 64)
		if perr != nil {
			continue
		}

		spec := queue.Spec{Kind: rec.Kind, ID: rec.ID}
		if rec.Params != "" {
			_ = json.Unmarshal([]byte(rec.Params), &spec.Params)
		}

		out = append(out, PersistedTask{Seq: seq, Spec: spec, EnqueuedAt: rec.EnqueuedAt})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })

	return out, nil
}
