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

// Spec is the serializable description of a task, sufficient to rebuild its
// Run closure after a restart. The queue never interprets it; it hands the Spec
// to the Persister so the manager can round-trip queued/running work across
// restarts. Params carries kind-specific fields (e.g. generate targets/force).
type Spec struct {
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Params map[string]string `json:"params,omitempty"`
}

// Persister records queued/running tasks durably so they survive a restart.
// The queue calls Save when a task is enqueued and Remove once it reaches a
// terminal state (done/error/canceled) or is dropped. Implementations must be
// safe for concurrent use; errors are logged by the implementation, not the
// queue. A nil Persister disables persistence.
type Persister interface {
	Save(seq uint64, spec Spec, enqueuedAt time.Time)
	Remove(seq uint64)
}

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
	// Spec, when set, is the serializable description persisted so the task can
	// be rebuilt after a restart. Tasks with a zero Spec (empty Kind) are not
	// persisted; use it for transient/coordinator work.
	Spec Spec
	// seq, when non-zero, restores a task's sequence number instead of
	// allocating a new one. Used only by Restore when re-enqueuing persisted
	// work so the UI keeps stable ids across a restart.
	seq uint64
	// noPersist suppresses the Save callback for this submit. Restore sets it
	// because the record is already on disk; re-saving would be redundant.
	noPersist bool
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
	spec       Spec
	persisted  bool // a Save was issued for this task and no Remove yet
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

	// persist records queued/running tasks so they survive a restart. It is set
	// once via SetPersister before any Submit and is read without the lock.
	persist Persister

	wake           chan struct{}
	dispatcherDone chan struct{}
}

// SetPersister installs the durable store for queued/running tasks. It must be
// called during setup, before Submit/Restore are used. A nil persister keeps
// persistence disabled.
func (q *Queue) SetPersister(p Persister) {
	q.mu.Lock()
	q.persist = p
	q.mu.Unlock()
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

	// Restore reuses the persisted seq; a normal Submit allocates the next one
	// and keeps q.seq monotonic across restarts.
	seq := t.seq
	if seq == 0 {
		q.seq++
		seq = q.seq
	} else if seq > q.seq {
		q.seq = seq
	}

	enqueuedAt := time.Now()
	nt := &task{
		seq:        seq,
		id:         t.ID,
		kind:       t.Kind,
		title:      firstNonEmpty(t.Title, t.Kind),
		key:        t.Key,
		spec:       t.Spec,
		run:        t.Run,
		state:      StateQueued,
		enqueuedAt: enqueuedAt,
		handle:     &Handle{done: make(chan struct{})},
	}

	q.pending = append(q.pending, nt)
	if t.Key != "" {
		q.byKey[t.Key] = nt
	}

	// Persist queued work so it survives a restart. A Restore replaying an
	// existing record skips the Save but still tracks the task so its terminal
	// Remove fires; tasks without a serializable spec are never persisted.
	switch {
	case nt.spec.Kind == "":
		// transient/coordinator task: not persisted
	case t.noPersist:
		nt.persisted = true
	case q.persist != nil:
		nt.persisted = true
		q.persist.Save(nt.seq, nt.spec, enqueuedAt)
	}

	q.wakeUp()

	return nt.handle
}

// Restore re-enqueues a task read from the Persister after a restart, reusing
// its original seq so UI ids stay stable and skipping the Save (the record
// already exists on disk). Terminal states still trigger Remove. It behaves
// like Submit otherwise, including dedup by Key.
func (q *Queue) Restore(seq uint64, t Task) *Handle {
	t.seq = seq
	t.noPersist = true

	return q.Submit(t)
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
		q.removePersistedLocked(t)
		q.pushRecentLocked(t)
		close(t.handle.done)
		n++
	}
	q.pending = kept

	return n
}

// CancelSeq drops the single queued (not-yet-started) task with the given seq,
// marking it canceled and closing its handle. It reports whether a matching
// queued task was found and removed; a running or already-finished task is not
// affected (cancel a running job through its own context via the manager).
func (q *Queue) CancelSeq(seq uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, t := range q.pending {
		if t.seq != seq {
			continue
		}

		q.pending = append(q.pending[:i], q.pending[i+1:]...)
		t.state = StateCanceled
		t.endedAt = time.Now()
		q.removeKeyLocked(t)
		q.removePersistedLocked(t)
		q.pushRecentLocked(t)
		close(t.handle.done)

		return true
	}

	return false
}

// RunningID returns the ID of the currently running task with the given seq and
// true when such a task is running. The manager uses it to translate a
// per-task cancel (by seq) into canceling that task's underlying job context,
// since the queue itself does not own job cancellation.
func (q *Queue) RunningID(seq uint64) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if t, ok := q.active[seq]; ok {
		return t.id, true
	}

	return "", false
}

// Bump moves the queued task with the given seq to the front of the backlog so
// it is the next to start when a slot frees (or immediately if one is free). It
// reports whether a matching queued task was found. A running or finished task
// cannot be bumped.
func (q *Queue) Bump(seq uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, t := range q.pending {
		if t.seq != seq {
			continue
		}

		if i > 0 {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			q.pending = append([]*task{t}, q.pending...)
			q.wakeUp()
		}

		return true
	}

	return false
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
		// The whole queue is shutting down: this run was interrupted by the
		// process exiting, not by the user. Keep its durable record so the
		// task is re-enqueued (as queued) on the next start instead of lost.
		t.state = StateCanceled
		t.err = err
	case err != nil:
		t.state = StateError
		t.err = err
		q.removePersistedLocked(t)
	default:
		t.state = StateDone
		q.removePersistedLocked(t)
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
		// Durable records are intentionally KEPT here: these tasks are being
		// canceled only because the process is shutting down, so they must be
		// restored (as queued) on the next start rather than discarded.
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

// removePersistedLocked drops a task's durable record once it reaches a
// terminal state, so restart never replays finished/canceled work. A no-op for
// tasks that were never persisted.
func (q *Queue) removePersistedLocked(t *task) {
	if q.persist != nil && t.persisted {
		q.persist.Remove(t.seq)
		t.persisted = false
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
