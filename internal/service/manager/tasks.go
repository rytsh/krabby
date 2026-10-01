package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/taskstore"
	"github.com/rytsh/krabby/internal/service/websource"
)

// Background task kinds submitted to the central queue. They classify work in
// the Activity UI and namespace the dedup keys.
const (
	taskKindRefresh   = "refresh"
	taskKindGenerate  = "generate"
	taskKindWebSync   = "websync"
	taskKindWebImport = "webimport"
	taskKindReindex   = "reindex"
	taskKindAPISync   = "apisync"
)

// SetTaskConcurrency updates the central work queue's concurrency limit live.
// A value <= 0 falls back to the queue default. Called at startup and whenever
// the setting changes in the UI/REST/MCP.
func (m *Manager) SetTaskConcurrency(n int) {
	m.queue.SetLimit(n)
}

// TaskSnapshot returns the current queue state (limit, running/queued counts,
// and queued/running/recent tasks) for the Activity UI.
func (m *Manager) TaskSnapshot() queue.Snapshot {
	return m.queue.Snapshot()
}

// ClearTaskHistory removes finished tasks from the Activity UI without
// affecting queued or running work.
func (m *Manager) ClearTaskHistory() {
	m.queue.ClearHistory()
}

// CancelPendingTasks removes all queued work while allowing running tasks to
// continue.
func (m *Manager) CancelPendingTasks() int {
	return m.queue.CancelAllPending()
}

// BumpTask moves the queued task with the given seq to the front of the backlog
// so it starts next when a slot frees. It reports whether a matching queued
// task was found (a running or finished task cannot be bumped).
func (m *Manager) BumpTask(seq uint64) bool {
	return m.queue.Bump(seq)
}

// CancelTask cancels the task with the given seq. A queued task is dropped from
// the backlog; a running task has its underlying job aborted (its context is
// cancelled, killing the in-flight git/graph-build/index work). It reports whether
// a matching task was found in either state.
func (m *Manager) CancelTask(seq uint64) bool {
	return m.queue.CancelSeq(seq)
}

// CancelTasks cancels every queued and running task for a repository or web
// source scope and returns how many tasks cancellation was requested for.
func (m *Manager) CancelTasks(id string) int {
	return m.queue.CancelID(id)
}

// TaskState returns the live queue state for a repository or web-source scope.
func (m *Manager) TaskState(id string) string {
	return string(m.queue.LiveState(id))
}

// TaskStore persists queued/running tasks so the work queue survives a restart.
// It is the queue.Persister plus a List used to replay records on startup.
type TaskStore interface {
	queue.Persister
	List(ctx context.Context) ([]taskstore.PersistedTask, error)
}

// SetTaskStore installs the durable task store and closes the queue's bootstrap
// gates. Call it once at startup before any trigger enqueues work. RestoreTasks
// enables durable submissions after listing, seeding and replaying every record;
// StartTaskQueue releases dispatch after all startup dependencies are ready.
func (m *Manager) SetTaskStore(store TaskStore) {
	m.taskStore = store
	m.queue.SetPersister(store)
}

// RestoreTasks re-enqueues tasks that were queued (or running) when the process
// last stopped. Records are read in FIFO seq order and rebuilt from their spec;
// a task that was running before the restart comes back as queued (its previous
// run died with the process). Unknown or malformed specs are dropped so a bad
// record cannot wedge startup. Duplicate restored keys keep the oldest record
// and delete every coalesced record, avoiding durable orphans. Dispatch remains
// paused until StartTaskQueue so restored closures cannot race startup setup. It
// is a no-op when no store is configured.
func (m *Manager) RestoreTasks(ctx context.Context) error {
	if m.taskStore == nil {
		return nil
	}

	tasks, err := m.taskStore.List(ctx)
	if err != nil {
		return err
	}
	var maxSeq uint64
	for _, pt := range tasks {
		maxSeq = max(maxSeq, pt.Seq)
	}
	// Seed from the complete listing, including malformed records that will be
	// removed below, so no new task can ever reuse a sequence observed on disk.
	m.queue.SeedSequence(maxSeq)

	restored := 0
	var cleanupErrs []error
	for _, pt := range tasks {
		if err := ctx.Err(); err != nil {
			return err
		}

		t, ok := m.rebuildTask(pt.Spec)
		if !ok {
			// Drop records we can no longer interpret so they are not retried
			// on every restart.
			if rerr := m.taskStore.Remove(pt.Seq); rerr != nil {
				cleanupErrs = append(cleanupErrs, rerr)
			}

			continue
		}

		h, result := m.queue.Restore(pt.Seq, t)
		switch result {
		case queue.RestoreAccepted:
			restored++
		case queue.RestoreCoalesced:
			// Restore dedup intentionally keeps the first (lowest-seq) task. The
			// later durable row must be deleted or it will be replayed forever.
			if rerr := m.taskStore.Remove(pt.Seq); rerr != nil {
				cleanupErrs = append(cleanupErrs, rerr)
			}
		case queue.RestoreRejected:
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := h.Err(); err != nil {
				return fmt.Errorf("restore persisted task %d; %w", pt.Seq, err)
			}

			return fmt.Errorf("restore persisted task %d rejected", pt.Seq)
		}
	}
	if err := errors.Join(cleanupErrs...); err != nil {
		return fmt.Errorf("clean persisted task records; %w", err)
	}

	m.queue.CompleteRestore()

	if restored > 0 {
		slog.Info("restored persisted background tasks", "count", restored)
	}

	return nil
}

// StartTaskQueue releases restored and startup-generated work for dispatch.
// Call it only after RestoreTasks and after every dependency used by rebuilt Run
// closures has been configured.
func (m *Manager) StartTaskQueue() {
	m.queue.StartDispatch()
}

// rebuildTask reconstructs a queue.Task (including its Run closure) from a
// persisted spec. It reports ok=false for specs whose target no longer exists
// or whose kind is unknown, so the caller can drop the record.
func (m *Manager) rebuildTask(spec queue.Spec) (queue.Task, bool) {
	switch spec.Kind {
	case taskKindRefresh:
		return m.refreshTask(spec.ID, splitTargets(spec.Params["skip"])), true

	case taskKindGenerate:
		targets := splitTargets(spec.Params["targets"])
		if len(targets) == 0 {
			return queue.Task{}, false
		}
		force := spec.Params["force"] == "true"

		return m.generateTask(spec.ID, targets, force), true

	case taskKindWebSync:
		// Persisted websync IDs are the scope key ("web:<name>"); recover the
		// collection name for the rebuilt closure.
		name := websource.CollectionName(spec.ID)
		if name == "" {
			return queue.Task{}, false
		}

		return m.webSyncTaskMode(name, spec.Params["force_full"] == "true"), true

	case taskKindAPISync:
		// Persisted apisync IDs are the scope key ("api:<name>"); recover the
		// service name for the rebuilt closure.
		name := apicatalog.ServiceName(spec.ID)
		if name == "" {
			return queue.Task{}, false
		}

		return m.apiSyncTaskMode(name, spec.Params["force_full"] == "true"), true

	case taskKindReindex:
		if spec.ID == "*" {
			return m.reindexAllTask(), true
		}

		return m.reindexTask(spec.ID), true

	default:
		return queue.Task{}, false
	}
}

// RefreshWait pulls and rebuilds a repository in the background and waits until
// the run finishes or ctx is done, then returns the latest repo record. done
// reports whether the refresh completed within the wait; when false the build
// continues detached and the record reflects the in-progress state.
// A build failure is surfaced via the record's Status/LastError rather than a
// Go error; enqueue and unexpected lookup failures return an error.
// skip names stages this run must not execute, on top of the repo's persisted
// skip_stages override.
func (m *Manager) RefreshWait(ctx context.Context, id string, skip ...string) (*registry.Repo, bool, error) {
	// The refresh runs on the manager lifecycle context: a client that stops
	// waiting (client-side tool timeout, ctrl+c, MCP cancellation) must not
	// kill the clone/build mid-flight.
	h := m.submitRefresh(id, skip)
	if err := h.Err(); err != nil {
		return nil, false, fmt.Errorf("enqueue refresh for %s; %w", id, err)
	}

	done := false
	select {
	case <-h.Done():
		done = true
	case <-ctx.Done():
	}

	// Read the record even after a caller cancel so the in-progress (or
	// terminal) state is always returned.
	repo, err := m.reg.Get(context.WithoutCancel(ctx), id)
	if err != nil {
		return nil, done, err
	}

	if repo == nil {
		return nil, done, fmt.Errorf("repo %s not found", id)
	}

	return repo, done, nil
}

// refreshTask builds the queue task for a repo refresh, including its Spec so
// the work is persisted and can be rebuilt after a restart. skip names stages
// this run must not execute, on top of the repo's persisted skip_stages.
func (m *Manager) refreshTask(id string, skip []string) queue.Task {
	skipped := newRunSkips(skip).list()

	title := "Refresh " + id
	// The skip set is part of the dedup key so a partial refresh never swallows
	// a full one: coalescing "refresh, but not docs" onto a plain "refresh"
	// would silently do less work than the caller asked for.
	key := taskKindRefresh + ":" + id
	var params map[string]string

	if len(skipped) > 0 {
		joined := strings.Join(skipped, ",")
		title += " (skip " + joined + ")"
		key += ":skip=" + joined
		params = map[string]string{"skip": joined}
	}

	return queue.Task{
		ID:    id,
		Kind:  taskKindRefresh,
		Title: title,
		Key:   key,
		Spec:  queue.Spec{Kind: taskKindRefresh, ID: id, Params: params},
		Run: func(ctx context.Context) error {
			if err := m.Refresh(ctx, id, skipped...); err != nil {
				slog.Error("refresh repo", "repo", id, "skip", skipped, "error", err)

				return err
			}

			return nil
		},
	}
}

// submitRefresh enqueues a repo refresh on the central work queue and returns
// its handle. Concurrent refreshes for the same repo and skip set coalesce onto
// one queued task (queue dedup), and the queue bounds how many refreshes run at
// once.
func (m *Manager) submitRefresh(id string, skip []string) *queue.Handle {
	return m.queue.Submit(m.refreshTask(id, skip))
}

// TriggerRefresh queues a background refresh for a repo on the central work
// queue. Concurrent triggers for the same repo and skip set coalesce, and the
// queue's concurrency limit bounds how many repos refresh at once. skip names
// stages this run must not execute, on top of the repo's persisted skip_stages
// override; scheduled and webhook-driven refreshes pass none. A synchronous
// queue persistence failure is returned.
func (m *Manager) TriggerRefresh(id string, skip ...string) error {
	return m.submitRefresh(id, skip).Err()
}

// generateTask builds the queue task for a selective generation, including its
// Spec (targets + force) so the work is persisted and rebuildable after a
// restart.
func (m *Manager) generateTask(id string, targets []string, force bool) queue.Task {
	return queue.Task{
		ID:    id,
		Kind:  taskKindGenerate,
		Title: fmt.Sprintf("Generate %s for %s", strings.Join(targets, ", "), id),
		Key:   fmt.Sprintf("%s:%s:%s:%t", taskKindGenerate, id, strings.Join(targets, ","), force),
		Spec: queue.Spec{
			Kind: taskKindGenerate,
			ID:   id,
			Params: map[string]string{
				"targets": strings.Join(targets, ","),
				"force":   strconv.FormatBool(force),
			},
		},
		Run: func(ctx context.Context) error {
			if err := m.Generate(ctx, id, targets, force); err != nil {
				slog.Error("generate", "repo", id, "targets", targets, "error", err)

				return err
			}

			return nil
		},
	}
}

// submitGenerate enqueues a selective generation on the central work queue and
// returns its handle. Identical requests (same repo, targets and force) coalesce
// onto one queued task.
func (m *Manager) submitGenerate(id string, targets []string, force bool) *queue.Handle {
	return m.queue.Submit(m.generateTask(id, targets, force))
}

// splitTargets parses a persisted comma-separated generate targets list,
// dropping blanks. The empty string yields no targets.
func splitTargets(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}

// TriggerGenerate queues a background selective generation for a repo. When
// force is true the docs stage ignores its incremental caches. A synchronous
// queue persistence failure is returned.
func (m *Manager) TriggerGenerate(id string, targets []string, force bool) error {
	return m.submitGenerate(id, targets, force).Err()
}

// GenerateWait runs the selected generation stages for a repo in the background
// and waits until the run finishes or ctx is done, then returns the latest repo
// record. done reports whether the generation completed within the wait; when
// false the run continues detached and the record reflects the in-progress
// state. Stage failures are surfaced via the record's Status/LastError rather
// than a Go error; enqueue and unexpected lookup failures return an error.
func (m *Manager) GenerateWait(ctx context.Context, id string, targets []string, force bool) (*registry.Repo, bool, error) {
	// The generation runs on the manager lifecycle context: a client that stops
	// waiting (client-side tool timeout, ctrl+c, MCP cancellation) must not
	// kill the build mid-flight.
	h := m.submitGenerate(id, targets, force)
	if err := h.Err(); err != nil {
		return nil, false, fmt.Errorf("enqueue generate for %s; %w", id, err)
	}

	done := false
	select {
	case <-h.Done():
		done = true
	case <-ctx.Done():
	}

	// Read the record even after a caller cancel so the in-progress (or
	// terminal) state is always returned.
	repo, err := m.reg.Get(context.WithoutCancel(ctx), id)
	if err != nil {
		return nil, done, err
	}

	if repo == nil {
		return nil, done, fmt.Errorf("repo %s not found", id)
	}

	return repo, done, nil
}

// TriggerReindexAll rebuilds optional docs/code indexes for every ready repo
// and web source without fetching git or rebuilding graph output. It is used
// after a live settings update because an ordinary refresh intentionally exits
// early when the repository commit has not changed.
//
// A lightweight coordinator task lists the work and enqueues one reindex task
// per repo/collection, so the global concurrency limit — not the repo count —
// governs how many run at once. A limit of 1 reindexes sequentially (the
// previous behavior, which avoided multiplying LLM/embedder load); a higher
// limit fans out within that bound.
func (m *Manager) TriggerReindexAll() error {
	return m.queue.Submit(m.reindexAllTask()).Err()
}

// reindexAllTask builds the reindex coordinator task with a Spec so a restart
// replays the whole reindex (which then re-enqueues per-repo/per-source work).
func (m *Manager) reindexAllTask() queue.Task {
	return queue.Task{
		ID:    "*",
		Kind:  taskKindReindex,
		Title: "Reindex all repositories and sources",
		Key:   taskKindReindex + ":all",
		Spec:  queue.Spec{Kind: taskKindReindex, ID: "*"},
		Run:   m.reindexAll,
	}
}

// reindexTask builds a deduplicated reindex task for one repo, one web-source
// collection or one API-catalog service, carrying a Spec so a restart replays
// it. The rebuilt closure dispatches on the id's shape the same way, which is
// why the scope prefixes have to be unambiguous: a repo id can never contain
// ':', so a prefixed key is always one of the non-repo kinds.
func (m *Manager) reindexTask(id string) queue.Task {
	if name := websource.CollectionName(id); name != "" {
		scope := id

		return queue.Task{
			ID:    scope,
			Kind:  taskKindReindex,
			Title: "Reindex " + scope,
			Key:   taskKindReindex + ":" + scope,
			Spec:  queue.Spec{Kind: taskKindReindex, ID: scope},
			Run: func(ctx context.Context) error {
				defer m.lockKey(scope)()

				m.indexAll(ctx, m.webCorpus(name))

				return nil
			},
		}
	}

	if name := apicatalog.ServiceName(id); name != "" {
		scope := id

		return queue.Task{
			ID:    scope,
			Kind:  taskKindReindex,
			Title: "Reindex " + scope,
			Key:   taskKindReindex + ":" + scope,
			Spec:  queue.Spec{Kind: taskKindReindex, ID: scope},
			Run: func(ctx context.Context) error {
				defer m.lockKey(scope)()

				m.indexAll(ctx, m.apiCorpus(name))

				return nil
			},
		}
	}

	return queue.Task{
		ID:    id,
		Kind:  taskKindReindex,
		Title: "Reindex " + id,
		Key:   taskKindReindex + ":" + id,
		Spec:  queue.Spec{Kind: taskKindReindex, ID: id},
		Run:   func(ctx context.Context) error { return m.reindexRepo(ctx, id) },
	}
}

// scheduleReindex enqueues a deduplicated background reindex of one repo's
// docs/code indexes. The queue key collapses repeats, so calling it while an
// identical reindex is already queued/running is a no-op.
func (m *Manager) scheduleReindex(id string) error {
	return m.queue.Submit(m.reindexTask(id)).Err()
}
