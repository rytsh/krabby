package manager

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

// newLockTestManager builds a Manager with a registry and a real queue, which
// is what the override writers need to schedule their follow-up rebuild.
func newLockTestManager(t *testing.T) (*Manager, *registry.Registry) {
	t.Helper()

	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	q := queue.New(context.Background(), 1)
	t.Cleanup(q.Close)

	return &Manager{reg: reg, reposDir: t.TempDir(), queue: q}, reg
}

// waitLockFree reports whether the key's lock becomes acquirable within the
// timeout, and leaves it released.
//
// It polls rather than checking once because SetRepoOverrides queues a rebuild
// that legitimately takes the same lock: a single TryLock could observe that
// worker and call a healthy lock leaked. What must never happen is the lock
// staying held forever, which is what this asserts.
func waitLockFree(t *testing.T, m *Manager, key string) bool {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.lockFor(key).TryLock() {
			m.lockFor(key).Unlock()

			return true
		}
		time.Sleep(10 * time.Millisecond)
	}

	return false
}

// lockKey is the only sanctioned way to take a per-key lock precisely because
// the pair "fetch mutex" / "unlock mutex" was once split across a function and
// one caller forgot to lock in between. Unlocking an unlocked mutex is a Go
// fatal error, not a panic: it kills the process, so no handler can contain it
// and every request in flight dies with it.
func TestLockKeyReturnsAWorkingUnlock(t *testing.T) {
	m, _ := newLockTestManager(t)

	unlock := m.lockKey("owner/repo")

	// Held: a second acquisition must not succeed.
	if m.lockFor("owner/repo").TryLock() {
		t.Fatal("lockKey returned without holding the lock")
	}

	unlock()

	if !m.lockFor("owner/repo").TryLock() {
		t.Fatal("the returned unlock did not release the lock")
	}
	m.lockFor("owner/repo").Unlock()
}

func TestLockKeyIsPerKey(t *testing.T) {
	m, _ := newLockTestManager(t)

	defer m.lockKey("owner/a")()

	// A different key must be unaffected, or one slow repository build would
	// serialize every other repository behind it.
	if !m.lockFor("owner/b").TryLock() {
		t.Fatal("locking one key blocked another")
	}
	m.lockFor("owner/b").Unlock()
}

// SetRepoOverrides took the per-repo mutex without locking it and then
// unlocked it, taking the process down on every call. Exercising the real path
// is the regression guard: the fatal aborts the test binary outright.
func TestSetRepoOverridesDoesNotCorruptTheRepoLock(t *testing.T) {
	m, reg := newLockTestManager(t)
	ctx := context.Background()

	repo := &registry.Repo{ID: "owner/deploy", URL: "https://git/owner/deploy"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	updated, err := m.SetRepoOverrides(ctx, repo.ID, registry.Overrides{
		IncludeExtra: []string{"**/*.yaml"},
	})
	if err != nil {
		t.Fatalf("SetRepoOverrides: %v", err)
	}
	if len(updated.Overrides.IncludeExtra) != 1 {
		t.Fatalf("overrides not stored: %+v", updated.Overrides)
	}

	// The lock must end up free: leaving it held would deadlock the rebuild
	// this very call just queued.
	if !waitLockFree(t, m, repo.ID) {
		t.Fatal("SetRepoOverrides left the repo lock held")
	}
}

// Saving the same form twice is the ordinary UI behaviour, and each save takes
// and releases the lock again.
func TestSetRepoOverridesRepeatedCallsKeepTheLockUsable(t *testing.T) {
	m, reg := newLockTestManager(t)
	ctx := context.Background()

	repo := &registry.Repo{ID: "owner/deploy", URL: "https://git/owner/deploy"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	over := registry.Overrides{
		Exclude:            []string{"_assets/**"},
		DocsMaxSourceBytes: 262144,
		SkipStages:         []string{registry.StageGraph},
	}

	for i := range 3 {
		if _, err := m.SetRepoOverrides(ctx, repo.ID, over); err != nil {
			t.Fatalf("SetRepoOverrides #%d: %v", i, err)
		}
	}

	if !waitLockFree(t, m, repo.ID) {
		t.Fatal("repo lock left held after repeated saves")
	}
}

// Concurrent saves are reachable from the UI and the MCP tool at once. They
// must serialize on the repo lock rather than race it.
func TestSetRepoOverridesConcurrent(t *testing.T) {
	m, reg := newLockTestManager(t)
	ctx := context.Background()

	repo := &registry.Repo{ID: "owner/deploy", URL: "https://git/owner/deploy"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)

	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := m.SetRepoOverrides(ctx, repo.ID, registry.Overrides{
				DocsMaxSourceBytes: 1024 * (i + 1),
			})
			errs <- err
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent SetRepoOverrides deadlocked")
	}

	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("SetRepoOverrides: %v", err)
		}
	}

	if !waitLockFree(t, m, repo.ID) {
		t.Fatal("repo lock left held after concurrent saves")
	}
}
