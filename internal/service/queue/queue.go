// Package queue provides a central, bounded work queue for krabby's background
// tasks (repository refresh/generate, web-source sync, reindex-all).
//
// Previously every trigger spawned its own unbounded goroutine, so enqueuing
// many repositories at once launched an unbounded number of concurrent git
// clones, graphify builds, LLM calls and embedder requests — overloading the
// host and "clogging" the pipeline. This package funnels all of that work
// through a single queue whose concurrency is governed by one runtime-mutable
// limit (exposed in the settings UI): at most Limit tasks run at a time and the
// rest wait in a FIFO backlog.
//
// The limit is enforced by the queue's own dispatcher so it can be changed live
// (errgroup.SetLimit cannot be modified while goroutines are active). An
// errgroup tracks the launched task goroutines and drains them on shutdown.
package queue

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// State is the lifecycle state of a task, surfaced to the UI.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateError    State = "error"
	StateCanceled State = "canceled"
)

// DefaultConcurrency is used until a value is loaded from settings and whenever
// a non-positive limit is requested.
const DefaultConcurrency = 3

// maxRecent bounds the finished-task history kept for the UI.
const maxRecent = 30

// Task describes one unit of background work submitted to the queue.
type Task struct {
	// ID is the primary subject of the work, typically a repo id or web-source
	// scope key ("web:<name>"). Used for display and by CancelPending.
	ID string
	// Kind classifies the work ("refresh", "generate", "reindex", "websync").
	Kind string
	// Title is a short human-readable label for the UI; falls back to Kind.
	Title string
	// Key deduplicates queued work: if a *pending* task with the same non-empty
	// Key already exists, Submit coalesces onto it instead of enqueuing a copy.
	// A task that has already started running no longer holds its key, so one
	// follow-up request may queue behind a running task. An empty Key disables
	// dedup for that task.
	Key string
	// Run performs the work. ctx is derived from the queue's base context and
	// is cancelled on queue shutdown.
	Run func(ctx context.Context) error
}

// Handle lets a caller wait for a submitted task to finish.
type Handle struct {
	done chan struct{}
}

// Done is closed when the task has finished, or immediately when the task was
// rejected (queue closing) or coalesced onto an already-finished task.
func (h *Handle) Done() <-chan struct{} { return h.done }

func closedHandle() *Handle {
	h := &Handle{done: make(chan struct{})}
	close(h.done)

	return h
}

// Item is an immutable snapshot of one task's state for the UI.
type Item struct {
	Seq        uint64    `json:"seq"`
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title,omitempty"`
	State      State     `json:"state"`
	Error      string    `json:"error,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	EndedAt    time.Time `json:"ended_at,omitzero"`
}

// Snapshot is the queue state exposed to the UI: the configured concurrency
// limit, live counters, and the queued/running/recently-finished tasks.
type Snapshot struct {
	Limit   int    `json:"limit"`
	Running int    `json:"running"`
	Pending int    `json:"pending"`
	Tasks   []Item `json:"tasks"` // queued + running + recent finished, newest first
}

// task is the internal, mutable representation of queued work.
type task struct {
	seq        uint64
	id         string
	kind       string
	title      string
	key        string
	run        func(ctx context.Context) error
	state      State
	err        error
	enqueuedAt time.Time
	startedAt  time.Time
	endedAt    time.Time
	handle     *Handle
}

func (t *task) item() Item {
	it := Item{
		Seq:        t.seq,
		ID:         t.id,
		Kind:       t.kind,
		Title:      t.title,
		State:      t.state,
		EnqueuedAt: t.enqueuedAt,
		StartedAt:  t.startedAt,
		EndedAt:    t.endedAt,
	}
	if t.err != nil {
		it.Error = t.err.Error()
	}

	return it
}

// Queue is a bounded FIFO work queue with a runtime-mutable concurrency limit.
type Queue struct {
	ctx    context.Context //nolint:containedctx // bounds the lifetime of all queued work
	cancel context.CancelFunc
	eg     *errgroup.Group

	mu      sync.Mutex
	limit   int
	running int
	seq     uint64
	pending []*task
	active  map[uint64]*task // currently running
	byKey   map[string]*task // pending tasks only, for dedup and CancelPending
	recent  []*task          // finished tasks, oldest first, capped at maxRecent
	closed  bool

	wake           chan struct{}
	dispatcherDone chan struct{}
}

// New creates a queue bound to baseCtx and starts its dispatcher. A limit <= 0
// uses DefaultConcurrency.
func New(baseCtx context.Context, limit int) *Queue {
	if limit <= 0 {
		limit = DefaultConcurrency
	}

	ctx, cancel := context.WithCancel(baseCtx)
	q := &Queue{
		ctx:            ctx,
		cancel:         cancel,
		eg:             &errgroup.Group{},
		limit:          limit,
		active:         map[uint64]*task{},
		byKey:          map[string]*task{},
		wake:           make(chan struct{}, 1),
		dispatcherDone: make(chan struct{}),
	}

	go q.dispatch()

	return q
}

// SetLimit changes how many tasks may run concurrently, effective immediately.
// A limit <= 0 falls back to DefaultConcurrency. Raising the limit lets waiting
// tasks start at once; lowering it takes effect as running tasks finish (it
// never interrupts work already in progress).
func (q *Queue) SetLimit(n int) {
	if n <= 0 {
		n = DefaultConcurrency
	}

	q.mu.Lock()
	q.limit = n
	q.mu.Unlock()

	q.wakeUp()
}

// Limit returns the current concurrency limit.
func (q *Queue) Limit() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.limit
}

// Submit enqueues a task and returns a handle to wait for its completion. When
// a pending task with the same Key already exists the call coalesces onto it
// and returns that task's handle. When the queue is shutting down (or Run is
// nil) an already-closed handle is returned and nothing is enqueued.
func (q *Queue) Submit(t Task) *Handle {
	if t.Run == nil {
		return closedHandle()
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return closedHandle()
	}

	if t.Key != "" {
		if existing, ok := q.byKey[t.Key]; ok {
			return existing.handle
		}
	}

	q.seq++
	nt := &task{
		seq:        q.seq,
		id:         t.ID,
		kind:       t.Kind,
		title:      firstNonEmpty(t.Title, t.Kind),
		key:        t.Key,
		run:        t.Run,
		state:      StateQueued,
		enqueuedAt: time.Now(),
		handle:     &Handle{done: make(chan struct{})},
	}

	q.pending = append(q.pending, nt)
	if t.Key != "" {
		q.byKey[t.Key] = nt
	}

	q.wakeUp()

	return nt.handle
}

// CancelPending drops queued (not-yet-started) tasks whose ID matches id,
// marking them canceled and closing their handles. Running tasks are
// unaffected (cancel those through their own job context). It reports how many
// queued tasks were removed.
func (q *Queue) CancelPending(id string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	kept := q.pending[:0]
	n := 0
	for _, t := range q.pending {
		if t.id != id {
			kept = append(kept, t)

			continue
		}

		t.state = StateCanceled
		t.endedAt = time.Now()
		q.removeKeyLocked(t)
		q.pushRecentLocked(t)
		close(t.handle.done)
		n++
	}
	q.pending = kept

	return n
}

// Snapshot returns the current queue state for the UI.
func (q *Queue) Snapshot() Snapshot {
	q.mu.Lock()
	defer q.mu.Unlock()

	items := make([]Item, 0, len(q.active)+len(q.pending)+len(q.recent))
	for _, t := range q.active {
		items = append(items, t.item())
	}
	for _, t := range q.pending {
		items = append(items, t.item())
	}
	for _, t := range q.recent {
		items = append(items, t.item())
	}
	// Newest first: running/queued tasks (highest seq) lead, recent history
	// trails.
	sort.Slice(items, func(i, j int) bool { return items[i].Seq > items[j].Seq })

	return Snapshot{
		Limit:   q.limit,
		Running: q.running,
		Pending: len(q.pending),
		Tasks:   items,
	}
}

// Close stops accepting new tasks, cancels queued tasks, cancels the context of
// running tasks and waits for them to finish. It is idempotent.
func (q *Queue) Close() {
	q.stopAccepting()
	q.cancel()

	<-q.dispatcherDone
	_ = q.eg.Wait()
}

// dispatch is the single scheduler goroutine. It starts as many pending tasks
// as the current limit allows, then waits to be woken by a submit, a completion
// or a limit change.
func (q *Queue) dispatch() {
	defer close(q.dispatcherDone)

	for {
		q.mu.Lock()
		for !q.closed && len(q.pending) > 0 && q.running < q.limit {
			t := q.pending[0]
			q.pending = q.pending[1:]
			q.removeKeyLocked(t) // a running task no longer dedups new submits

			q.running++
			t.state = StateRunning
			t.startedAt = time.Now()
			q.active[t.seq] = t

			q.launchLocked(t)
		}
		closed := q.closed
		q.mu.Unlock()

		if closed {
			return
		}

		select {
		case <-q.wake:
		case <-q.ctx.Done():
			q.stopAccepting()
		}
	}
}

// launchLocked starts a task goroutine. It is called with q.mu held; the
// goroutine's completion handler re-acquires the lock once the dispatcher
// releases it.
func (q *Queue) launchLocked(t *task) {
	q.eg.Go(func() error {
		err := safeRun(q.ctx, t.run)
		q.finish(t, err)

		// Errors are recorded per task; never propagate so one failure cannot
		// tear down the shared errgroup.
		return nil
	})
}

func (q *Queue) finish(t *task, err error) {
	q.mu.Lock()
	q.running--
	delete(q.active, t.seq)
	t.endedAt = time.Now()
	switch {
	case err != nil && q.ctx.Err() != nil:
		t.state = StateCanceled
		t.err = err
	case err != nil:
		t.state = StateError
		t.err = err
	default:
		t.state = StateDone
	}
	q.pushRecentLocked(t)
	q.mu.Unlock()

	close(t.handle.done)
	q.wakeUp()
}

// stopAccepting marks the queue closed and cancels every queued task. Running
// tasks are left to finish (Close cancels their context and waits).
func (q *Queue) stopAccepting() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()

		return
	}
	q.closed = true
	for _, t := range q.pending {
		t.state = StateCanceled
		t.endedAt = time.Now()
		q.removeKeyLocked(t)
		q.pushRecentLocked(t)
		close(t.handle.done)
	}
	q.pending = nil
	q.mu.Unlock()

	q.wakeUp()
}

func (q *Queue) removeKeyLocked(t *task) {
	if t.key != "" && q.byKey[t.key] == t {
		delete(q.byKey, t.key)
	}
}

func (q *Queue) pushRecentLocked(t *task) {
	q.recent = append(q.recent, t)
	if len(q.recent) > maxRecent {
		q.recent = q.recent[len(q.recent)-maxRecent:]
	}
}

func (q *Queue) wakeUp() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func safeRun(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("task panicked: %v", r)
		}
	}()

	return fn(ctx)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}

	return b
}
