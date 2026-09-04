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
	"errors"
	"fmt"
	"log/slog"
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

var (
	// ErrBootstrapIncomplete rejects durable work submitted before persisted
	// tasks have been fully listed, sequence-seeded and restored.
	ErrBootstrapIncomplete = errors.New("durable queue bootstrap is incomplete")
	// ErrClosed rejects work submitted after queue shutdown has begun.
	ErrClosed = errors.New("queue is closed")
)

// RestoreResult distinguishes a durable row that was restored from one that
// coalesced with an older row or could not be accepted at all.
type RestoreResult uint8

const (
	RestoreAccepted RestoreResult = iota
	RestoreCoalesced
	RestoreRejected
)

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
// safe for concurrent use. A nil Persister disables persistence.
type Persister interface {
	Save(seq uint64, spec Spec, enqueuedAt time.Time) error
	Remove(seq uint64) error
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
	once sync.Once
	mu   sync.Mutex
	err  error
}

// Done is closed when the task has finished, or immediately when the task was
// rejected (queue closing) or coalesced onto an already-finished task.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Err reports the task error recorded so far and is final after Done is closed.
// Because Submit does not return until Save finishes, a durable submission whose
// Save failed exposes that error as soon as Submit returns and never dispatches.
func (h *Handle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.err
}

func (h *Handle) complete(err error) {
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	h.once.Do(func() { close(h.done) })
}

func closedHandle() *Handle {
	h := &Handle{done: make(chan struct{})}
	h.complete(nil)

	return h
}

func failedHandle(err error) *Handle {
	h := &Handle{done: make(chan struct{})}
	h.complete(err)

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
	persisting bool // Save is in progress; dispatch must wait for it
	persisted  bool // Save succeeded and no Remove has succeeded yet
	run        func(ctx context.Context) error
	state      State
	err        error
	enqueuedAt time.Time
	startedAt  time.Time
	endedAt    time.Time
	handle     *Handle
	cancel     context.CancelFunc
	canceled   bool
	inRecent   bool
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

	// Installing a Persister creates a two-phase startup gate. Restore first
	// seeds/replays every durable record, CompleteRestore then allows new durable
	// submissions, and StartDispatch finally lets any work run. This prevents a
	// fresh low sequence from overwriting an unseen record and keeps restored
	// closures dormant until their manager dependencies are configured.
	persistenceReady bool
	dispatchReady    bool

	// persist records queued/running tasks so they survive a restart. It is set
	// once via SetPersister before any durable Submit.
	persist Persister
	// persistWG covers Save calls that can outlive their Submit caller relative
	// to Close (for example when Close races a blocked Save).
	persistWG sync.WaitGroup

	wake           chan struct{}
	dispatcherDone chan struct{}
}

// SetPersister installs the durable store for queued/running tasks. A non-nil
// persister closes both bootstrap gates: callers must SeedSequence, replay all
// records with Restore, call CompleteRestore, and finally call StartDispatch.
// Until CompleteRestore, ordinary durable submissions are rejected rather than
// risking a sequence collision. A nil persister keeps persistence disabled.
func (q *Queue) SetPersister(p Persister) {
	q.mu.Lock()
	q.persist = p
	q.persistenceReady = p == nil
	q.dispatchReady = p == nil
	q.mu.Unlock()

	q.wakeUp()
}

// SeedSequence raises the sequence allocator to at least max. Bootstrap must
// call it with the maximum sequence from the full persisted task listing before
// any new durable submission is enabled.
func (q *Queue) SeedSequence(max uint64) {
	q.mu.Lock()
	if max > q.seq {
		q.seq = max
	}
	q.mu.Unlock()
}

// CompleteRestore marks the durable listing and replay phase complete. New
// durable tasks may now be saved with sequence values above the seeded maximum,
// but nothing dispatches until StartDispatch is called.
func (q *Queue) CompleteRestore() {
	q.mu.Lock()
	q.persistenceReady = true
	q.mu.Unlock()
}

// StartDispatch releases the bootstrap dispatch gate. It is separate from
// CompleteRestore so startup-generated work can coalesce with restored pending
// tasks before either is allowed to run.
func (q *Queue) StartDispatch() {
	q.mu.Lock()
	if !q.persistenceReady {
		q.mu.Unlock()

		return
	}
	q.dispatchReady = true
	q.mu.Unlock()

	q.wakeUp()
}

// New creates a queue bound to baseCtx and starts its dispatcher. A limit <= 0
// uses DefaultConcurrency.
func New(baseCtx context.Context, limit int) *Queue {
	if limit <= 0 {
		limit = DefaultConcurrency
	}

	ctx, cancel := context.WithCancel(baseCtx)
	q := &Queue{
		ctx:              ctx,
		cancel:           cancel,
		eg:               &errgroup.Group{},
		limit:            limit,
		active:           map[uint64]*task{},
		byKey:            map[string]*task{},
		persistenceReady: true,
		dispatchReady:    true,
		wake:             make(chan struct{}, 1),
		dispatcherDone:   make(chan struct{}),
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

// Submit enqueues a task and returns a handle to wait for its completion. When
// a pending task with the same Key already exists the call coalesces onto it
// and returns that task's handle. When the queue is shutting down (or Run is
// nil) an already-closed handle is returned and nothing is enqueued.
func (q *Queue) Submit(t Task) *Handle {
	h, _ := q.submit(t)

	return h
}

// submit reports whether the task was accepted, coalesced with existing
// pending work, or rejected before enqueue.
func (q *Queue) submit(t Task) (*Handle, RestoreResult) {
	if t.Run == nil {
		return closedHandle(), RestoreRejected
	}

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()

		return failedHandle(ErrClosed), RestoreRejected
	}

	needsSave := t.Spec.Kind != "" && !t.noPersist && q.persist != nil
	if needsSave && !q.persistenceReady {
		q.mu.Unlock()

		slog.Error("reject durable task before queue bootstrap", "kind", t.Spec.Kind, "id", t.Spec.ID)

		return failedHandle(ErrBootstrapIncomplete), RestoreRejected
	}

	if t.Key != "" {
		if existing, ok := q.byKey[t.Key]; ok {
			q.mu.Unlock()

			return existing.handle, RestoreCoalesced
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
		persisting: needsSave,
		run:        t.Run,
		state:      StateQueued,
		enqueuedAt: enqueuedAt,
		handle:     &Handle{done: make(chan struct{})},
	}

	q.pending = append(q.pending, nt)
	if t.Key != "" {
		q.byKey[t.Key] = nt
	}

	// Restore replays an existing durable record. New durable work remains at
	// the front of the FIFO in a non-dispatchable state while Save runs outside
	// q.mu; this preserves submit order without making snapshots and controls
	// wait for taskstore's timeout.
	if t.noPersist && nt.spec.Kind != "" {
		nt.persisted = true
	}
	p := q.persist
	if needsSave {
		q.persistWG.Add(1)
	}
	q.mu.Unlock()

	if !needsSave {
		q.wakeUp()

		return nt.handle, RestoreAccepted
	}

	saveErr := p.Save(nt.seq, nt.spec, enqueuedAt)
	removeAfterSave := false
	closeAfterSave := false

	q.mu.Lock()
	nt.persisting = false
	if saveErr != nil {
		q.removePendingLocked(nt)
		q.removeKeyLocked(nt)
		nt.endedAt = time.Now()
		nt.err = fmt.Errorf("persist queued task: %w", saveErr)
		if !nt.canceled {
			nt.state = StateError
		}
		q.pushRecentLocked(nt)
		closeAfterSave = true
	} else {
		nt.persisted = true
		switch {
		case nt.canceled:
			// Explicit cancellation raced Save. Delete only after Save has
			// completed, otherwise the late write would create an orphan.
			removeAfterSave = true
			closeAfterSave = true
		case q.closed:
			// Shutdown keeps the successfully saved record for next startup.
			closeAfterSave = true
		}
	}
	q.mu.Unlock()

	if saveErr != nil {
		slog.Error("persist queued task", "seq", nt.seq, "error", saveErr)
	}
	if removeAfterSave {
		q.removePersisted(p, nt)
	}
	if closeAfterSave {
		nt.handle.complete(nt.err)
	}
	q.wakeUp()
	q.persistWG.Done()

	return nt.handle, RestoreAccepted
}

// Restore re-enqueues a task read from the Persister after a restart, reusing
// its original seq so UI ids stay stable and skipping the Save (the record
// already exists on disk). Terminal states still trigger Remove. It behaves
// like Submit otherwise, including dedup by Key. A coalesced result tells the
// caller to remove the losing durable row; a rejected result must be retained.
func (q *Queue) Restore(seq uint64, t Task) (*Handle, RestoreResult) {
	t.seq = seq
	t.noPersist = true

	return q.submit(t)
}

// CancelPending drops queued (not-yet-started) tasks whose ID matches id,
// marking them canceled and closing their handles. Running tasks are
// unaffected (cancel those through their own job context). It reports how many
// queued tasks were removed.
func (q *Queue) CancelPending(id string) int {
	q.mu.Lock()

	kept := q.pending[:0]
	var remove, complete []*task
	p := q.persist
	n := 0
	for _, t := range q.pending {
		if t.id != id {
			kept = append(kept, t)

			continue
		}

		t.state = StateCanceled
		t.endedAt = time.Now()
		t.canceled = true
		q.removeKeyLocked(t)
		q.pushRecentLocked(t)
		if !t.persisting {
			complete = append(complete, t)
			if p != nil && t.persisted {
				remove = append(remove, t)
				q.persistWG.Add(1)
			}
		}
		n++
	}
	q.pending = kept
	q.mu.Unlock()

	q.removeAndComplete(p, remove, complete)
	q.wakeUp()

	return n
}

// CancelAllPending drops every queued task without affecting running work. The
// canceled tasks remain in recent history so the cancellation is visible.
func (q *Queue) CancelAllPending() int {
	q.mu.Lock()

	n := len(q.pending)
	var remove, complete []*task
	p := q.persist
	for _, t := range q.pending {
		t.state = StateCanceled
		t.endedAt = time.Now()
		t.canceled = true
		q.removeKeyLocked(t)
		q.pushRecentLocked(t)
		if !t.persisting {
			complete = append(complete, t)
			if p != nil && t.persisted {
				remove = append(remove, t)
				q.persistWG.Add(1)
			}
		}
	}
	q.pending = nil
	q.mu.Unlock()

	q.removeAndComplete(p, remove, complete)
	q.wakeUp()

	return n
}

// CancelID cancels every queued and running task whose ID matches id. It
// returns the number of tasks cancellation was requested for.
func (q *Queue) CancelID(id string) int {
	q.mu.Lock()

	kept := q.pending[:0]
	var remove, complete []*task
	p := q.persist
	n := 0
	for _, t := range q.pending {
		if t.id != id {
			kept = append(kept, t)

			continue
		}

		t.state = StateCanceled
		t.endedAt = time.Now()
		t.canceled = true
		q.removeKeyLocked(t)
		q.pushRecentLocked(t)
		if !t.persisting {
			complete = append(complete, t)
			if p != nil && t.persisted {
				remove = append(remove, t)
				q.persistWG.Add(1)
			}
		}
		n++
	}
	q.pending = kept

	for _, t := range q.active {
		if t.id != id || t.canceled {
			continue
		}

		t.canceled = true
		if t.cancel != nil {
			t.cancel()
		}
		n++
	}
	q.mu.Unlock()

	q.removeAndComplete(p, remove, complete)
	q.wakeUp()

	return n
}

// LiveState returns the current state of work for id. Running wins over queued
// when both a current run and a follow-up task exist.
func (q *Queue) LiveState(id string) State {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, t := range q.active {
		if t.id == id && !t.canceled {
			return StateRunning
		}
	}
	for _, t := range q.pending {
		if t.id == id {
			return StateQueued
		}
	}

	return ""
}

// CancelSeq cancels the single queued or running task with the given seq. A
// queued task is removed immediately; a running task's context is canceled and
// reaches the terminal canceled state when its Run function returns.
func (q *Queue) CancelSeq(seq uint64) bool {
	q.mu.Lock()

	for i, t := range q.pending {
		if t.seq != seq {
			continue
		}

		q.pending = append(q.pending[:i], q.pending[i+1:]...)
		t.state = StateCanceled
		t.endedAt = time.Now()
		t.canceled = true
		q.removeKeyLocked(t)
		q.pushRecentLocked(t)
		p := q.persist
		complete := !t.persisting
		remove := complete && p != nil && t.persisted
		if remove {
			q.persistWG.Add(1)
		}
		q.mu.Unlock()

		if remove {
			q.removePersisted(p, t)
			q.persistWG.Done()
		}
		if complete {
			t.handle.complete(t.err)
		}
		q.wakeUp()

		return true
	}

	if t, ok := q.active[seq]; ok {
		t.canceled = true
		if t.cancel != nil {
			t.cancel()
		}
		q.mu.Unlock()

		return true
	}
	q.mu.Unlock()

	return false
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
	// Live work leads by queue order. Finished work follows by completion time:
	// a long-running task can have an older seq while still being the latest
	// history entry.
	sort.Slice(items, func(i, j int) bool {
		iLive := items[i].State == StateRunning || items[i].State == StateQueued
		jLive := items[j].State == StateRunning || items[j].State == StateQueued
		if iLive != jLive {
			return iLive
		}
		if iLive || items[i].EndedAt.Equal(items[j].EndedAt) {
			return items[i].Seq > items[j].Seq
		}

		return items[i].EndedAt.After(items[j].EndedAt)
	})

	return Snapshot{
		Limit:   q.limit,
		Running: q.running,
		Pending: len(q.pending),
		Tasks:   items,
	}
}

// ClearHistory removes finished tasks from the UI history. Queued and running
// tasks are unaffected.
func (q *Queue) ClearHistory() {
	q.mu.Lock()
	q.recent = nil
	q.mu.Unlock()
}

// Close stops accepting new tasks, cancels queued tasks, cancels the context of
// running tasks and waits for them to finish. It is idempotent.
func (q *Queue) Close() {
	q.stopAccepting()
	q.cancel()

	<-q.dispatcherDone
	_ = q.eg.Wait()
	q.persistWG.Wait()
}

// dispatch is the single scheduler goroutine. It starts as many pending tasks
// as the current limit allows, then waits to be woken by a submit, a completion
// or a limit change.
func (q *Queue) dispatch() {
	defer close(q.dispatcherDone)

	for {
		q.mu.Lock()
		for !q.closed && q.dispatchReady && len(q.pending) > 0 && q.running < q.limit {
			t := q.pending[0]
			if t.persisting {
				break
			}
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
	ctx, cancel := context.WithCancel(q.ctx)
	t.cancel = cancel
	q.eg.Go(func() error {
		err := safeRun(ctx, t.run)
		// Capture this before the local cleanup cancel. Only cancellation that
		// reached Run itself means shutdown interrupted durable work.
		runCtxErr := ctx.Err()
		cancel()
		q.finish(t, err, runCtxErr)

		// Errors are recorded per task; never propagate so one failure cannot
		// tear down the shared errgroup.
		return nil
	})
}

func (q *Queue) finish(t *task, err, runCtxErr error) {
	q.mu.Lock()
	q.running--
	delete(q.active, t.seq)
	t.endedAt = time.Now()
	remove := false
	p := q.persist
	switch {
	case t.canceled:
		t.state = StateCanceled
		t.err = nil
		remove = p != nil && t.persisted
	case runCtxErr != nil && q.ctx.Err() != nil:
		// The whole queue is shutting down: this run was interrupted by the
		// process exiting, not by the user. Keep its durable record so the
		// task is re-enqueued (as queued) on the next start instead of lost.
		// runCtxErr is captured before the task-local cancel, so a task that
		// merely finishes while the queue is closed still removes its record.
		t.state = StateCanceled
		t.err = err
	case err != nil:
		t.state = StateError
		t.err = err
		remove = p != nil && t.persisted
	default:
		t.state = StateDone
		remove = p != nil && t.persisted
	}
	if remove {
		q.persistWG.Add(1)
	}
	q.pushRecentLocked(t)
	q.mu.Unlock()

	q.wakeUp()
	if remove {
		q.removePersisted(p, t)
		q.persistWG.Done()
	}
	t.handle.complete(t.err)
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
	var complete []*task
	for _, t := range q.pending {
		t.state = StateCanceled
		t.endedAt = time.Now()
		q.removeKeyLocked(t)
		// Durable records are intentionally KEPT here: these tasks are being
		// canceled only because the process is shutting down, so they must be
		// restored (as queued) on the next start rather than discarded.
		q.pushRecentLocked(t)
		if !t.persisting {
			complete = append(complete, t)
		}
	}
	q.pending = nil
	q.mu.Unlock()
	for _, t := range complete {
		t.handle.complete(t.err)
	}

	q.wakeUp()
}

func (q *Queue) removeKeyLocked(t *task) {
	if t.key != "" && q.byKey[t.key] == t {
		delete(q.byKey, t.key)
	}
}

// removePersisted drops a terminal task's durable record without holding q.mu.
// A failed cleanup remains visible on the task and is logged; persisted stays
// true because the record may still be present and replay on restart.
func (q *Queue) removePersisted(p Persister, t *task) {
	err := p.Remove(t.seq)

	q.mu.Lock()
	if err == nil {
		t.persisted = false
	} else {
		t.err = errors.Join(t.err, fmt.Errorf("remove persisted task: %w", err))
		if t.state == StateDone {
			t.state = StateError
		}
	}
	q.mu.Unlock()

	if err != nil {
		slog.Error("remove persisted task", "seq", t.seq, "error", err)
	}
}

func (q *Queue) removeAndComplete(p Persister, remove, complete []*task) {
	for _, t := range remove {
		q.removePersisted(p, t)
		q.persistWG.Done()
	}
	for _, t := range complete {
		t.handle.complete(t.err)
	}
}

func (q *Queue) removePendingLocked(target *task) {
	for i, t := range q.pending {
		if t == target {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)

			return
		}
	}
}

func (q *Queue) pushRecentLocked(t *task) {
	if t.inRecent {
		return
	}
	t.inRecent = true
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
