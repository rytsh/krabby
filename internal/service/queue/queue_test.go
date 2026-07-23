package queue

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("condition not met within %s", timeout)
}

func TestBoundedConcurrency(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 2)
	defer q.Close()

	var current, peak int32
	var mu sync.Mutex
	release := make(chan struct{})
	started := make(chan struct{}, 6)

	run := func(context.Context) error {
		n := atomic.AddInt32(&current, 1)
		mu.Lock()
		if n > peak {
			peak = n
		}
		mu.Unlock()
		started <- struct{}{}
		<-release
		atomic.AddInt32(&current, -1)

		return nil
	}

	handles := make([]*Handle, 0, 6)
	for i := range 6 {
		handles = append(handles, q.Submit(Task{ID: fmt.Sprintf("t%d", i), Kind: "test", Run: run}))
	}

	// Exactly the limit should start; a third must not.
	<-started
	<-started
	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 2 })

	select {
	case <-started:
		t.Fatal("a third task started while the limit was 2")
	case <-time.After(50 * time.Millisecond):
	}

	if got := q.Snapshot().Pending; got != 4 {
		t.Fatalf("pending = %d, want 4", got)
	}

	close(release)
	for _, h := range handles {
		<-h.Done()
	}

	mu.Lock()
	defer mu.Unlock()
	if peak != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak)
	}
}

func TestSetLimitRaises(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	release := make(chan struct{})
	var running int32

	run := func(context.Context) error {
		atomic.AddInt32(&running, 1)
		<-release
		atomic.AddInt32(&running, -1)

		return nil
	}

	for i := range 3 {
		q.Submit(Task{ID: fmt.Sprintf("t%d", i), Run: run})
	}

	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&running) == 1 })

	q.SetLimit(3)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&running) == 3 })

	close(release)
	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 0 })
}

func TestDedupPendingCoalesces(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	release := make(chan struct{})
	block := q.Submit(Task{ID: "busy", Key: "busy", Run: func(context.Context) error {
		<-release

		return nil
	}})

	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 1 })

	var runs int32
	dup := func(context.Context) error {
		atomic.AddInt32(&runs, 1)

		return nil
	}

	h1 := q.Submit(Task{ID: "target", Key: "dup", Run: dup})
	h2 := q.Submit(Task{ID: "target", Key: "dup", Run: dup})

	if h1 != h2 {
		t.Fatal("submits with the same key should return the same handle")
	}
	if got := q.Snapshot().Pending; got != 1 {
		t.Fatalf("pending = %d, want 1 (coalesced)", got)
	}

	close(release)
	<-block.Done()
	<-h1.Done()

	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("dup runs = %d, want 1", got)
	}
}

func TestRunningTaskDoesNotDedupNewSubmit(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	first := make(chan struct{})
	release := make(chan struct{})
	var runs int32

	run := func(context.Context) error {
		if atomic.AddInt32(&runs, 1) == 1 {
			close(first)
		}
		<-release

		return nil
	}

	q.Submit(Task{ID: "a", Key: "same", Run: run})
	<-first // first task is now running and no longer holds its key

	// A second submit with the same key must queue behind it, not coalesce.
	h2 := q.Submit(Task{ID: "a", Key: "same", Run: run})
	if got := q.Snapshot().Pending; got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}

	close(release)
	<-h2.Done()

	if got := atomic.LoadInt32(&runs); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

func TestCancelPending(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	release := make(chan struct{})
	q.Submit(Task{ID: "busy", Key: "busy", Run: func(context.Context) error {
		<-release

		return nil
	}})
	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 1 })

	var ran int32
	pending := q.Submit(Task{ID: "target", Run: func(context.Context) error {
		atomic.AddInt32(&ran, 1)

		return nil
	}})

	if n := q.CancelPending("target"); n != 1 {
		t.Fatalf("CancelPending = %d, want 1", n)
	}

	select {
	case <-pending.Done():
	case <-time.After(time.Second):
		t.Fatal("canceled task handle was not closed")
	}

	close(release)
	waitFor(t, time.Second, func() bool { return q.Snapshot().Running == 0 })

	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("canceled task ran %d times, want 0", got)
	}

	// The canceled task appears in the snapshot history as canceled.
	found := false
	for _, it := range q.Snapshot().Tasks {
		if it.ID == "target" {
			found = true
			if it.State != StateCanceled {
				t.Fatalf("target state = %q, want %q", it.State, StateCanceled)
			}
		}
	}
	if !found {
		t.Fatal("canceled task missing from snapshot history")
	}
}

func TestSnapshotStatesAndError(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	h := q.Submit(Task{ID: "boom", Kind: "test", Run: func(context.Context) error {
		return fmt.Errorf("kaboom")
	}})
	<-h.Done()

	var item *Item
	for _, it := range q.Snapshot().Tasks {
		if it.ID == "boom" {
			it := it
			item = &it
		}
	}

	if item == nil {
		t.Fatal("finished task missing from snapshot")
	}
	if item.State != StateError {
		t.Fatalf("state = %q, want %q", item.State, StateError)
	}
	if item.Error != "kaboom" {
		t.Fatalf("error = %q, want %q", item.Error, "kaboom")
	}
}

func TestSubmitAfterCloseIsRejected(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	q.Close()

	var ran int32
	h := q.Submit(Task{ID: "late", Run: func(context.Context) error {
		atomic.AddInt32(&ran, 1)

		return nil
	}})

	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("rejected handle should be closed immediately")
	}

	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("task ran after close: %d", got)
	}
}

func TestCloseCancelsRunningContext(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)

	sawCancel := make(chan struct{})
	started := make(chan struct{})
	q.Submit(Task{ID: "long", Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(sawCancel)

		return ctx.Err()
	}})

	<-started
	q.Close() // cancels running task contexts and waits for them

	select {
	case <-sawCancel:
	case <-time.After(time.Second):
		t.Fatal("running task context was not cancelled on Close")
	}
}

func TestPanicInTaskIsContained(t *testing.T) {
	t.Parallel()

	q := New(context.Background(), 1)
	defer q.Close()

	h := q.Submit(Task{ID: "panic", Run: func(context.Context) error {
		panic("boom")
	}})
	<-h.Done()

	// A subsequent task must still run: the queue survived the panic.
	var ran int32
	h2 := q.Submit(Task{ID: "after", Run: func(context.Context) error {
		atomic.AddInt32(&ran, 1)

		return nil
	}})
	<-h2.Done()

	if got := atomic.LoadInt32(&ran); got != 1 {
		t.Fatalf("task after panic ran %d times, want 1", got)
	}
}
