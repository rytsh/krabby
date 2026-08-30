package queue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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

func (f *fakePersister) Save(seq uint64, spec Spec, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved[seq] = spec

	return nil
}

func (f *fakePersister) Remove(seq uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.saved, seq)

	return nil
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

func bootstrapPersister(q *Queue, p Persister, maxSeq uint64) {
	q.SetPersister(p)
	q.SeedSequence(maxSeq)
	q.CompleteRestore()
	q.StartDispatch()
}

// TestPersistSavedThenRemovedOnDone verifies a task is saved on submit and its
// record dropped once it completes successfully.
func TestPersistSavedThenRemovedOnDone(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	p := newFakePersister()
	bootstrapPersister(q, p, 0)

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
	bootstrapPersister(q, p, 0)

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
	bootstrapPersister(q, p, 0)

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
	bootstrapPersister(q, p, 0)

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

func TestPersistRunningRecordKeptWhenShutdownInterruptsRunReturningNil(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	p := newFakePersister()
	bootstrapPersister(q, p, 0)

	started := make(chan struct{})
	q.Submit(Task{
		ID:   "running",
		Spec: Spec{Kind: "refresh", ID: "running"},
		Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()

			return nil
		},
	})
	<-started

	var seq uint64
	for _, item := range q.Snapshot().Tasks {
		if item.ID == "running" {
			seq = item.Seq
		}
	}
	if seq == 0 {
		t.Fatal("running task missing from snapshot")
	}

	q.Close()
	if !p.has(seq) {
		t.Fatal("running task record was removed when shutdown Run returned nil")
	}
}

func TestPersistRunningRecordRemovedWhenRunFinishesWhileQueueClosed(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	p := newFakePersister()
	bootstrapPersister(q, p, 0)

	started := make(chan struct{})
	release := make(chan struct{})
	h := q.Submit(Task{
		ID:   "running",
		Spec: Spec{Kind: "refresh", ID: "running"},
		Run: func(context.Context) error {
			close(started)
			<-release

			return nil
		},
	})
	<-started

	var seq uint64
	for _, item := range q.Snapshot().Tasks {
		if item.ID == "running" {
			seq = item.Seq
		}
	}
	if seq == 0 {
		t.Fatal("running task missing from snapshot")
	}

	// Close acceptance without canceling the run context, then let the run
	// finish. This deterministically exercises finish observing q.closed after
	// successful work rather than relying on Close's scheduling race.
	q.stopAccepting()
	close(release)
	<-h.Done()
	if p.has(seq) {
		t.Fatal("completed task record was retained merely because queue was closed")
	}
	q.Close()
}

func TestPersistRunningRecordRemovedOnExplicitUserCancellation(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()
	p := newFakePersister()
	bootstrapPersister(q, p, 0)

	started := make(chan struct{})
	h := q.Submit(Task{
		ID:   "running",
		Spec: Spec{Kind: "refresh", ID: "running"},
		Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()

			return nil
		},
	})
	<-started

	var seq uint64
	for _, item := range q.Snapshot().Tasks {
		if item.ID == "running" {
			seq = item.Seq
		}
	}
	if seq == 0 || !q.CancelSeq(seq) {
		t.Fatalf("cancel running task seq %d", seq)
	}
	<-h.Done()
	if p.has(seq) {
		t.Fatal("explicitly canceled running task record was retained")
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
	if err := p.Save(42, Spec{Kind: "refresh", ID: "r1"}, time.Now()); err != nil {
		t.Fatalf("seed persister: %v", err)
	}
	q.SetPersister(p)
	q.SeedSequence(42)

	var ran bool
	var mu sync.Mutex
	h, result := q.Restore(42, Task{
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
	if result != RestoreAccepted {
		t.Fatalf("restore result = %v, want accepted", result)
	}

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

	q.CompleteRestore()
	q.StartDispatch()
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
	bootstrapPersister(q, p, 0)

	h := q.Submit(Task{ID: "transient", Kind: "coordinator", Run: func(context.Context) error { return nil }})
	<-h.Done()

	if p.count() != 0 {
		t.Fatalf("transient task was persisted: %d records", p.count())
	}
}

type controlledPersister struct {
	saveStarted   chan struct{}
	saveRelease   chan struct{}
	saveErr       error
	removeStarted chan struct{}
	removeRelease chan struct{}
	removeErr     error
	saveOnce      sync.Once
	removeOnce    sync.Once
}

func (p *controlledPersister) Save(uint64, Spec, time.Time) error {
	if p.saveStarted != nil {
		p.saveOnce.Do(func() { close(p.saveStarted) })
	}
	if p.saveRelease != nil {
		<-p.saveRelease
	}

	return p.saveErr
}

func (p *controlledPersister) Remove(uint64) error {
	if p.removeStarted != nil {
		p.removeOnce.Do(func() { close(p.removeStarted) })
	}
	if p.removeRelease != nil {
		<-p.removeRelease
	}

	return p.removeErr
}

func TestDurableSubmitRejectedBeforeRestoreCompletes(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()
	p := newFakePersister()
	q.SetPersister(p)

	var ran atomic.Bool
	h := q.Submit(Task{
		ID:   "new",
		Spec: Spec{Kind: "refresh", ID: "new"},
		Run:  func(context.Context) error { ran.Store(true); return nil },
	})
	<-h.Done()

	if !errors.Is(h.Err(), ErrBootstrapIncomplete) {
		t.Fatalf("handle error = %v, want ErrBootstrapIncomplete", h.Err())
	}
	if ran.Load() {
		t.Fatal("task submitted before bootstrap ran")
	}
	if p.count() != 0 {
		t.Fatalf("pre-bootstrap task wrote %d records", p.count())
	}
}

func TestRestoredTaskWaitsForStartDispatch(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()
	p := newFakePersister()
	if err := p.Save(9, Spec{Kind: "refresh", ID: "old"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	q.SetPersister(p)
	q.SeedSequence(9)

	started := make(chan struct{})
	h, result := q.Restore(9, Task{
		ID:   "old",
		Key:  "refresh:old",
		Spec: Spec{Kind: "refresh", ID: "old"},
		Run: func(context.Context) error {
			close(started)

			return nil
		},
	})
	if result != RestoreAccepted {
		t.Fatalf("restore result = %v, want accepted", result)
	}
	q.CompleteRestore()

	select {
	case <-started:
		t.Fatal("restored task dispatched before StartDispatch")
	case <-time.After(30 * time.Millisecond):
	}

	q.StartDispatch()
	<-h.Done()
	if h.Err() != nil {
		t.Fatalf("restored task error: %v", h.Err())
	}
}

func TestRestoreDistinguishesCoalescedAndRejected(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	p := newFakePersister()
	q.SetPersister(p)
	q.SeedSequence(2)

	task := Task{
		ID:   "repo",
		Key:  "refresh:repo",
		Spec: Spec{Kind: "refresh", ID: "repo"},
		Run:  func(context.Context) error { return nil },
	}
	if _, result := q.Restore(1, task); result != RestoreAccepted {
		t.Fatalf("first restore result = %v, want accepted", result)
	}
	if _, result := q.Restore(2, task); result != RestoreCoalesced {
		t.Fatalf("duplicate restore result = %v, want coalesced", result)
	}

	q.Close()
	h, result := q.Restore(3, Task{
		ID:   "later",
		Spec: Spec{Kind: "refresh", ID: "later"},
		Run:  func(context.Context) error { return nil },
	})
	if result != RestoreRejected {
		t.Fatalf("closed restore result = %v, want rejected", result)
	}
	if !errors.Is(h.Err(), ErrClosed) {
		t.Fatalf("closed restore error = %v, want ErrClosed", h.Err())
	}
}

func TestSaveFailureIsTerminalAndObservable(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("disk unavailable")
	p := &controlledPersister{saveErr: wantErr}
	q := New(context.Background(), 1)
	defer q.Close()
	bootstrapPersister(q, p, 0)

	var ran atomic.Bool
	h := q.Submit(Task{
		ID:   "failed-save",
		Spec: Spec{Kind: "refresh", ID: "failed-save"},
		Run:  func(context.Context) error { ran.Store(true); return nil },
	})
	<-h.Done()

	if !errors.Is(h.Err(), wantErr) {
		t.Fatalf("handle error = %v, want wrapped %v", h.Err(), wantErr)
	}
	if ran.Load() {
		t.Fatal("task ran after Save failed")
	}

	found := false
	for _, item := range q.Snapshot().Tasks {
		if item.ID == "failed-save" {
			found = true
			if item.State != StateError || !strings.Contains(item.Error, wantErr.Error()) {
				t.Fatalf("failed-save item = %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("failed Save missing from queue history")
	}
}

func TestDelayedSaveDoesNotHoldQueueMutexOrDispatch(t *testing.T) {
	t.Parallel()

	p := &controlledPersister{
		saveStarted: make(chan struct{}),
		saveRelease: make(chan struct{}),
	}
	q := New(context.Background(), 1)
	defer q.Close()
	bootstrapPersister(q, p, 0)

	started := make(chan struct{})
	handleCh := make(chan *Handle, 1)
	go func() {
		handleCh <- q.Submit(Task{
			ID:   "slow-save",
			Spec: Spec{Kind: "refresh", ID: "slow-save"},
			Run: func(context.Context) error {
				close(started)

				return nil
			},
		})
	}()
	<-p.saveStarted

	snapshotCh := make(chan Snapshot, 1)
	go func() { snapshotCh <- q.Snapshot() }()
	select {
	case snap := <-snapshotCh:
		if snap.Pending != 1 || snap.Running != 0 {
			t.Fatalf("snapshot during Save = %+v, want one pending and none running", snap)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Snapshot blocked behind persistence Save")
	}
	select {
	case <-started:
		t.Fatal("task dispatched before Save completed")
	default:
	}

	close(p.saveRelease)
	h := <-handleCh
	<-h.Done()
}

func TestDelayedRemoveDoesNotHoldQueueMutex(t *testing.T) {
	t.Parallel()

	p := &controlledPersister{
		removeStarted: make(chan struct{}),
		removeRelease: make(chan struct{}),
	}
	q := New(context.Background(), 1)
	defer q.Close()
	bootstrapPersister(q, p, 0)

	h := q.Submit(Task{
		ID:   "slow-remove",
		Spec: Spec{Kind: "refresh", ID: "slow-remove"},
		Run:  func(context.Context) error { return nil },
	})
	<-p.removeStarted

	snapshotCh := make(chan Snapshot, 1)
	go func() { snapshotCh <- q.Snapshot() }()
	select {
	case snap := <-snapshotCh:
		if snap.Running != 0 {
			t.Fatalf("running during delayed Remove = %d, want 0", snap.Running)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Snapshot blocked behind persistence Remove")
	}

	close(p.removeRelease)
	<-h.Done()
}

func TestRemoveFailureIsObservable(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("delete failed")
	p := &controlledPersister{removeErr: wantErr}
	q := New(context.Background(), 1)
	defer q.Close()
	bootstrapPersister(q, p, 0)

	h := q.Submit(Task{
		ID:   "remove-failure",
		Spec: Spec{Kind: "refresh", ID: "remove-failure"},
		Run:  func(context.Context) error { return nil },
	})
	<-h.Done()
	if !errors.Is(h.Err(), wantErr) {
		t.Fatalf("handle error = %v, want wrapped %v", h.Err(), wantErr)
	}

	for _, item := range q.Snapshot().Tasks {
		if item.ID == "remove-failure" {
			if item.State != StateError || !strings.Contains(item.Error, wantErr.Error()) {
				t.Fatalf("remove-failure item = %+v", item)
			}

			return
		}
	}
	t.Fatal("Remove failure missing from queue history")
}
