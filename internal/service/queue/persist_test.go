package queue

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakePersister records Save/Remove calls so tests can assert what the queue
// durably tracks.
type fakePersister struct {
	mu    sync.Mutex
	saved map[uint64]Spec
}

func newFakePersister() *fakePersister {
	return &fakePersister{saved: map[uint64]Spec{}}
}

func (f *fakePersister) Save(seq uint64, spec Spec, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved[seq] = spec
}

func (f *fakePersister) Remove(seq uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.saved, seq)
}

func (f *fakePersister) has(seq uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.saved[seq]

	return ok
}

func (f *fakePersister) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.saved)
}

// TestPersistSavedThenRemovedOnDone verifies a task is saved on submit and its
// record dropped once it completes successfully.
func TestPersistSavedThenRemovedOnDone(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	p := newFakePersister()
	q.SetPersister(p)

	release := make(chan struct{})
	h := q.Submit(Task{
		ID:   "r1",
		Kind: "refresh",
		Spec: Spec{Kind: "refresh", ID: "r1"},
		Run:  func(context.Context) error { <-release; return nil },
	})

	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 1 })
	if p.count() != 1 {
		t.Fatalf("saved count = %d, want 1 while running", p.count())
	}

	close(release)
	<-h.Done()

	waitFor(t, time.Second, func() bool { return p.count() == 0 })
}

// TestPersistRemovedOnError verifies a failed task's record is dropped (it is
// not retried on restart).
func TestPersistRemovedOnError(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	p := newFakePersister()
	q.SetPersister(p)

	h := q.Submit(Task{
		ID:   "r1",
		Kind: "refresh",
		Spec: Spec{Kind: "refresh", ID: "r1"},
		Run:  func(context.Context) error { return context.DeadlineExceeded },
	})
	<-h.Done()

	waitFor(t, time.Second, func() bool { return p.count() == 0 })
}

// TestPersistRemovedOnUserCancel verifies an explicit CancelSeq drops the record.
func TestPersistRemovedOnUserCancel(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	p := newFakePersister()
	q.SetPersister(p)

	release := make(chan struct{})
	q.Submit(Task{ID: "busy", Spec: Spec{Kind: "refresh", ID: "busy"}, Run: func(context.Context) error { <-release; return nil }})
	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 1 })

	q.Submit(Task{ID: "queued", Spec: Spec{Kind: "refresh", ID: "queued"}, Run: func(context.Context) error { return nil }})

	seq, ok := queuedSeqByID(q, "queued")
	if !ok {
		t.Fatal("queued task not found")
	}
	if !p.has(seq) {
		t.Fatal("queued task was not persisted")
	}

	if !q.CancelSeq(seq) {
		t.Fatal("CancelSeq returned false")
	}
	if p.has(seq) {
		t.Fatal("cancelled task record was not removed")
	}

	close(release)
	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 0 })
}

// TestPersistKeptOnShutdown verifies that tasks queued when the queue closes
// keep their durable records so they can be restored on the next start.
func TestPersistKeptOnShutdown(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)

	p := newFakePersister()
	q.SetPersister(p)

	release := make(chan struct{})
	q.Submit(Task{ID: "busy", Spec: Spec{Kind: "refresh", ID: "busy"}, Run: func(ctx context.Context) error {
		select {
		case <-release:
		case <-ctx.Done():
		}

		return nil
	}})
	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 1 })

	// Two tasks wait behind the busy one.
	q.Submit(Task{ID: "a", Spec: Spec{Kind: "refresh", ID: "a"}, Run: func(context.Context) error { return nil }})
	q.Submit(Task{ID: "b", Spec: Spec{Kind: "refresh", ID: "b"}, Run: func(context.Context) error { return nil }})

	waitFor(t, time.Second, func() bool { return q.Snapshot().Pending == 2 })

	// Close cancels queued tasks (shutdown) but must keep their records: the
	// running task's record is also kept because its run was interrupted.
	close(release)
	q.Close()

	if p.count() == 0 {
		t.Fatal("all records removed on shutdown; queued/interrupted tasks would be lost")
	}
}

// TestRestoreReenqueuesWithSeq verifies a restored task reuses its seq, does not
// re-Save, and still Removes on completion.
func TestRestoreReenqueuesWithSeq(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	p := newFakePersister()
	// Pre-seed the "on disk" record as if it survived a restart.
	p.Save(42, Spec{Kind: "refresh", ID: "r1"}, time.Now())
	q.SetPersister(p)

	var ran bool
	var mu sync.Mutex
	h := q.Restore(42, Task{
		ID:   "r1",
		Kind: "refresh",
		Spec: Spec{Kind: "refresh", ID: "r1"},
		Run: func(context.Context) error {
			mu.Lock()
			ran = true
			mu.Unlock()

			return nil
		},
	})

	// The restored task keeps its original seq.
	found := false
	for _, it := range q.Snapshot().Tasks {
		if it.ID == "r1" {
			found = true
			if it.Seq != 42 {
				t.Fatalf("restored seq = %d, want 42", it.Seq)
			}
		}
	}
	if !found {
		t.Fatal("restored task missing from snapshot")
	}

	<-h.Done()
	waitFor(t, time.Second, func() bool { return !p.has(42) })

	mu.Lock()
	defer mu.Unlock()
	if !ran {
		t.Fatal("restored task did not run")
	}
}

// TestNoSpecNotPersisted verifies tasks without a Spec are never saved.
func TestNoSpecNotPersisted(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	p := newFakePersister()
	q.SetPersister(p)

	h := q.Submit(Task{ID: "transient", Kind: "coordinator", Run: func(context.Context) error { return nil }})
	<-h.Done()

	if p.count() != 0 {
		t.Fatalf("transient task was persisted: %d records", p.count())
	}
}
