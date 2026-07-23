package manager

import (
	"context"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/registry"
)

// blockQueue fills the queue's single slot with a task that blocks until the
// returned release func is called, so subsequently submitted tasks stay in the
// queued state and can be observed via Snapshot without racing execution.
func blockQueue(t *testing.T, q *queue.Queue) func() {
	t.Helper()

	release := make(chan struct{})
	started := make(chan struct{})
	q.Submit(queue.Task{
		ID:   "blocker",
		Kind: "blocker",
		Key:  "blocker",
		Run: func(ctx context.Context) error {
			close(started)
			<-release
			return nil
		},
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocker task did not start")
	}

	return func() { close(release) }
}

// waitForReindex polls the queue snapshot until a reindex task for id appears
// (in any state), or the deadline elapses.
func waitForReindex(t *testing.T, q *queue.Queue, id string) queue.Item {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, it := range q.Snapshot().Tasks {
			if it.Kind == taskKindReindex && it.ID == id {
				return it
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("no reindex task queued for %q", id)

	return queue.Item{}
}

// TestBuildDocsAndIndexEmptyBundleDefersReindex verifies that when the live
// docs/code bundle is unexpectedly empty (the transient state of a Close or a
// Configure swap), buildDocsAndIndex does not silently drop the work: it
// re-queues a reindex so a healthy bundle can pick it up later. This guards the
// regression where a settings change that coincided with a repo's post-graph
// index phase left docs/code_index permanently empty with no error.
func TestBuildDocsAndIndexEmptyBundleDefersReindex(t *testing.T) {
	ctx := context.Background()

	m := &Manager{
		// An empty bundle: every capability nil, exactly what Close installs
		// and what a mid-swap Configure can momentarily expose.
		docs:     &docsBundle{},
		queue:    queue.New(ctx, 1),
		activity: map[string]map[string]struct{}{},
	}
	t.Cleanup(m.queue.Close)

	release := blockQueue(t, m.queue) // occupy the slot so the reindex stays queued
	t.Cleanup(release)

	repo := &registry.Repo{ID: "owner/repo", Status: registry.StatusReady}

	m.buildDocsAndIndex(ctx, repo, true)

	it := waitForReindex(t, m.queue, repo.ID)
	if it.State != queue.StateQueued {
		t.Fatalf("reindex task state = %q, want %q", it.State, queue.StateQueued)
	}
}

// TestBuildDocsAndIndexEmptyBundleNoRequeueOnReindexPath verifies the reindex
// path (deferReindex=false) does not re-queue itself on an empty bundle, which
// would busy-loop the queue.
func TestBuildDocsAndIndexEmptyBundleNoRequeueOnReindexPath(t *testing.T) {
	ctx := context.Background()

	m := &Manager{
		docs:     &docsBundle{},
		queue:    queue.New(ctx, 1),
		activity: map[string]map[string]struct{}{},
	}
	t.Cleanup(m.queue.Close)

	release := blockQueue(t, m.queue)
	t.Cleanup(release)

	repo := &registry.Repo{ID: "owner/repo", Status: registry.StatusReady}

	m.buildDocsAndIndex(ctx, repo, false)

	// Give any (erroneous) submit a moment to land, then assert none did.
	time.Sleep(50 * time.Millisecond)
	for _, it := range m.queue.Snapshot().Tasks {
		if it.Kind == taskKindReindex && it.ID == repo.ID {
			t.Fatal("reindex path re-queued itself on empty bundle (busy-loop risk)")
		}
	}
}

// TestScheduleReindexDedups verifies the queue key collapses repeated reindex
// requests for the same repo into a single queued task.
func TestScheduleReindexDedups(t *testing.T) {
	ctx := context.Background()

	m := &Manager{
		queue: queue.New(ctx, 1),
	}
	t.Cleanup(m.queue.Close)

	release := blockQueue(t, m.queue) // occupy the slot so tasks stay queued
	t.Cleanup(release)

	m.scheduleReindex("owner/repo")
	m.scheduleReindex("owner/repo")
	m.scheduleReindex("owner/repo")

	count := 0
	for _, it := range m.queue.Snapshot().Tasks {
		if it.Kind == taskKindReindex && it.ID == "owner/repo" {
			count++
		}
	}

	if count != 1 {
		t.Fatalf("queued reindex tasks = %d, want 1 (deduped)", count)
	}
}
