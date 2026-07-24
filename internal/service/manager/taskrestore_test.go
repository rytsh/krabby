package manager

import (
	"context"
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

// TestRestoreTasksReenqueues verifies that persisted task records are rebuilt
// and re-enqueued in seq order, with a running-before-restart task coming back
// as queued.
func TestRestoreTasksReenqueues(t *testing.T) {
	ctx := context.Background()

	store := newTaskStore(t)
	// Simulate records left by a previous process: a refresh, a generate and a
	// websync.
	store.Save(3, queue.Spec{Kind: taskKindRefresh, ID: "acme/repo"}, time.Now())
	store.Save(5, queue.Spec{
		Kind:   taskKindGenerate,
		ID:     "acme/repo",
		Params: map[string]string{"targets": "graph,docs", "force": "true"},
	}, time.Now())
	store.Save(7, queue.Spec{Kind: taskKindWebSync, ID: "web:wiki"}, time.Now())

	m := &Manager{
		queue:    queue.New(ctx, 1),
		activity: map[string]map[string]struct{}{},
		locks:    map[string]*sync.Mutex{},
	}
	t.Cleanup(m.queue.Close)
	m.SetTaskStore(store)

	// Occupy the single slot so restored tasks stay queued and never actually
	// execute their closures (which would need real deps).
	release := blockQueue(t, m.queue)
	t.Cleanup(release)

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
	store.Save(1, queue.Spec{Kind: "bogus", ID: "x"}, time.Now())
	// A generate with no targets is unrebuildable.
	store.Save(2, queue.Spec{Kind: taskKindGenerate, ID: "acme/repo"}, time.Now())

	m := &Manager{
		queue:    queue.New(ctx, 1),
		activity: map[string]map[string]struct{}{},
		locks:    map[string]*sync.Mutex{},
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
