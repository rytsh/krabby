package manager

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/taskstore"
)

func newTaskStore(t *testing.T) *taskstore.Store {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatalf("open bw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s, err := taskstore.New(db)
	if err != nil {
		t.Fatalf("new taskstore: %v", err)
	}

	return s
}

func saveTask(t *testing.T, store *taskstore.Store, seq uint64, spec queue.Spec) {
	t.Helper()
	if err := store.Save(seq, spec, time.Now()); err != nil {
		t.Fatalf("save task %d: %v", seq, err)
	}
}

// TestRestoreTasksReenqueues verifies that persisted task records are rebuilt
// and re-enqueued in seq order, with a running-before-restart task coming back
// as queued.
func TestRestoreTasksReenqueues(t *testing.T) {
	ctx := context.Background()

	store := newTaskStore(t)
	// Simulate records left by a previous process: a refresh, a generate and a
	// websync.
	saveTask(t, store, 3, queue.Spec{Kind: taskKindRefresh, ID: "acme/repo"})
	saveTask(t, store, 5, queue.Spec{
		Kind:   taskKindGenerate,
		ID:     "acme/repo",
		Params: map[string]string{"targets": "graph,docs", "force": "true"},
	})
	saveTask(t, store, 7, queue.Spec{Kind: taskKindWebSync, ID: "web:wiki"})

	m := &Manager{
		queue:    queue.New(ctx, 1),
		activity: map[string]map[string]struct{}{},
	}
	t.Cleanup(m.queue.Close)
	m.SetTaskStore(store)

	if err := m.RestoreTasks(ctx); err != nil {
		t.Fatalf("RestoreTasks: %v", err)
	}

	bySeq := map[uint64]queue.Item{}
	for _, it := range m.queue.Snapshot().Tasks {
		bySeq[it.Seq] = it
	}

	for seq, wantKind := range map[uint64]string{3: taskKindRefresh, 5: taskKindGenerate, 7: taskKindWebSync} {
		it, ok := bySeq[seq]
		if !ok {
			t.Fatalf("restored task seq %d missing", seq)
		}
		if it.Kind != wantKind {
			t.Fatalf("seq %d kind = %q, want %q", seq, it.Kind, wantKind)
		}
		if it.State != queue.StateQueued {
			t.Fatalf("seq %d state = %q, want queued", seq, it.State)
		}
	}
}

// TestRestoreTasksDropsUnknownSpec verifies a malformed/unknown spec is removed
// from the store rather than retried forever.
func TestRestoreTasksDropsUnknownSpec(t *testing.T) {
	ctx := context.Background()

	store := newTaskStore(t)
	saveTask(t, store, 1, queue.Spec{Kind: "bogus", ID: "x"})
	// A generate with no targets is unrebuildable.
	saveTask(t, store, 2, queue.Spec{Kind: taskKindGenerate, ID: "acme/repo"})

	m := &Manager{
		queue:    queue.New(ctx, 1),
		activity: map[string]map[string]struct{}{},
	}
	t.Cleanup(m.queue.Close)
	m.SetTaskStore(store)

	if err := m.RestoreTasks(ctx); err != nil {
		t.Fatalf("RestoreTasks: %v", err)
	}

	// Both bad records must be gone from the store.
	left, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("unrebuildable records left = %d, want 0", len(left))
	}
}

func TestRestoreTasksRemovesDeduplicatedRecords(t *testing.T) {
	ctx := context.Background()
	store := newTaskStore(t)
	spec := queue.Spec{Kind: taskKindRefresh, ID: "acme/repo"}
	saveTask(t, store, 3, spec)
	saveTask(t, store, 8, spec)

	m := &Manager{queue: queue.New(ctx, 1)}
	t.Cleanup(m.queue.Close)
	m.SetTaskStore(store)

	if err := m.RestoreTasks(ctx); err != nil {
		t.Fatalf("RestoreTasks: %v", err)
	}

	left, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].Seq != 3 {
		t.Fatalf("persisted tasks = %+v, want only oldest seq 3", left)
	}

	snap := m.TaskSnapshot()
	if snap.Pending != 1 || len(snap.Tasks) != 1 || snap.Tasks[0].Seq != 3 {
		t.Fatalf("restored queue = %+v, want one pending seq 3", snap)
	}
}

func TestRestoreTasksSeedsNewTaskSequence(t *testing.T) {
	ctx := context.Background()
	store := newTaskStore(t)
	saveTask(t, store, 41, queue.Spec{Kind: taskKindRefresh, ID: "old/repo"})

	m := &Manager{queue: queue.New(ctx, 1)}
	t.Cleanup(m.queue.Close)
	m.SetTaskStore(store)

	if err := m.RestoreTasks(ctx); err != nil {
		t.Fatalf("RestoreTasks: %v", err)
	}
	if err := m.TriggerRefresh("new/repo"); err != nil {
		t.Fatalf("TriggerRefresh: %v", err)
	}

	left, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 2 || left[0].Seq != 41 || left[1].Seq != 42 {
		t.Fatalf("persisted sequences = %+v, want 41,42", left)
	}
	if left[0].Spec.ID != "old/repo" {
		t.Fatalf("original durable record was overwritten: %+v", left[0])
	}
}

type controlledTaskStore struct {
	mu       sync.Mutex
	records  []taskstore.PersistedTask
	removed  []uint64
	requests []queue.Spec
	saveErr  error
}

func (s *controlledTaskStore) Save(seq uint64, spec queue.Spec, enqueuedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = append(s.requests, spec)
	if s.saveErr != nil {
		return s.saveErr
	}
	s.records = append(s.records, taskstore.PersistedTask{Seq: seq, Spec: spec, EnqueuedAt: enqueuedAt})

	return nil
}

func (s *controlledTaskStore) Remove(seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.removed = append(s.removed, seq)
	for i := range s.records {
		if s.records[i].Seq == seq {
			s.records = append(s.records[:i], s.records[i+1:]...)

			break
		}
	}

	return nil
}

func (s *controlledTaskStore) List(context.Context) ([]taskstore.PersistedTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]taskstore.PersistedTask(nil), s.records...), nil
}

func (s *controlledTaskStore) snapshot() (records []taskstore.PersistedTask, removed []uint64, requests []queue.Spec) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append(records, s.records...), append(removed, s.removed...), append(requests, s.requests...)
}

func TestRestoreTasksRejectedClosedLeavesCurrentAndLaterRows(t *testing.T) {
	store := &controlledTaskStore{records: []taskstore.PersistedTask{
		{Seq: 1, Spec: queue.Spec{Kind: taskKindRefresh, ID: "first/repo"}},
		{Seq: 2, Spec: queue.Spec{Kind: taskKindRefresh, ID: "second/repo"}},
	}}
	m := &Manager{queue: queue.New(context.Background(), 1)}
	m.queue.Close()
	m.SetTaskStore(store)

	err := m.RestoreTasks(context.Background())
	if !errors.Is(err, queue.ErrClosed) {
		t.Fatalf("RestoreTasks error = %v, want ErrClosed", err)
	}
	records, removed, _ := store.snapshot()
	if len(records) != 2 || len(removed) != 0 {
		t.Fatalf("records = %+v, removed = %v; rejected restore must leave both intact", records, removed)
	}
}

func TestRestoreTasksCanceledLeavesRowsUnprocessed(t *testing.T) {
	store := &controlledTaskStore{records: []taskstore.PersistedTask{
		{Seq: 1, Spec: queue.Spec{Kind: taskKindRefresh, ID: "first/repo"}},
		{Seq: 2, Spec: queue.Spec{Kind: taskKindRefresh, ID: "second/repo"}},
	}}
	m := &Manager{queue: queue.New(context.Background(), 1)}
	t.Cleanup(m.queue.Close)
	m.SetTaskStore(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.RestoreTasks(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RestoreTasks error = %v, want context.Canceled", err)
	}
	records, removed, _ := store.snapshot()
	if len(records) != 2 || len(removed) != 0 {
		t.Fatalf("records = %+v, removed = %v; canceled restore processed durable rows", records, removed)
	}
}

func TestDurableManagerTriggersAndWaitsPropagateSaveFailure(t *testing.T) {
	wantErr := errors.New("task store unavailable")
	store := &controlledTaskStore{saveErr: wantErr}
	m := &Manager{queue: queue.New(context.Background(), 1)}
	t.Cleanup(m.queue.Close)
	m.SetTaskStore(store)
	if err := m.RestoreTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.StartTaskQueue()

	triggers := []struct {
		name string
		call func() error
	}{
		{"refresh", func() error { return m.TriggerRefresh("owner/repo") }},
		{"generate", func() error { return m.TriggerGenerate("owner/repo", []string{"graph"}, false) }},
		{"web", func() error { return m.TriggerWebRefresh("wiki") }},
		{"api", func() error { return m.TriggerAPIRefresh("billing") }},
		{"reindex", m.TriggerReindexAll},
	}
	for _, tt := range triggers {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, wantErr) {
				t.Fatalf("trigger error = %v, want wrapped %v", err, wantErr)
			}
		})
	}

	if _, done, err := m.RefreshWait(context.Background(), "owner/repo"); done || !errors.Is(err, wantErr) {
		t.Fatalf("RefreshWait = done %v, error %v; want false and wrapped Save error", done, err)
	}
	if _, done, err := m.GenerateWait(context.Background(), "owner/repo", []string{"graph"}, false); done || !errors.Is(err, wantErr) {
		t.Fatalf("GenerateWait = done %v, error %v; want false and wrapped Save error", done, err)
	}

	_, _, requests := store.snapshot()
	if len(requests) != len(triggers)+2 {
		t.Fatalf("Save requests = %d, want %d", len(requests), len(triggers)+2)
	}
	for _, spec := range requests {
		if strings.TrimSpace(spec.Kind) == "" {
			t.Fatalf("durable trigger submitted empty spec: %+v", spec)
		}
	}
}
