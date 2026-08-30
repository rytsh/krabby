package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/settings"
)

func bootstrapFailingTaskStore(t *testing.T, m *Manager, wantErr error) *controlledTaskStore {
	t.Helper()

	store := &controlledTaskStore{saveErr: wantErr}
	m.SetTaskStore(store)
	if err := m.RestoreTasks(context.Background()); err != nil {
		t.Fatalf("restore empty task store: %v", err)
	}
	m.StartTaskQueue()

	return store
}

func TestAddRepoMarksNewPendingRepoErrorWhenEnqueueSaveFails(t *testing.T) {
	wantErr := errors.New("task store unavailable")
	reg := newReindexRegistry(t)
	m := &Manager{
		reg:      reg,
		reposDir: t.TempDir(),
		queue:    queue.New(context.Background(), 1),
	}
	t.Cleanup(m.queue.Close)
	bootstrapFailingTaskStore(t, m, wantErr)

	repo, err := m.AddRepo(context.Background(), RepoSpec{URL: "https://github.com/acme/widgets.git"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("AddRepo error = %v, want wrapped %v", err, wantErr)
	}
	if repo == nil {
		t.Fatal("AddRepo did not return the newly registered error record")
	}

	stored, err := reg.Get(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Status != registry.StatusError || !strings.Contains(stored.LastError, wantErr.Error()) {
		t.Fatalf("stored repo = %+v, want error state naming enqueue failure", stored)
	}
}

func TestAddRepoWaitMarksNewPendingRepoErrorWhenEnqueueSaveFails(t *testing.T) {
	wantErr := errors.New("task store unavailable")
	reg := newReindexRegistry(t)
	m := &Manager{
		reg:      reg,
		reposDir: t.TempDir(),
		queue:    queue.New(context.Background(), 1),
	}
	t.Cleanup(m.queue.Close)
	bootstrapFailingTaskStore(t, m, wantErr)

	repo, done, err := m.AddRepoWait(context.Background(), RepoSpec{URL: "https://github.com/acme/waited.git"})
	if done || !errors.Is(err, wantErr) {
		t.Fatalf("AddRepoWait = done %v, error %v; want false and wrapped %v", done, err, wantErr)
	}
	if repo == nil || repo.Status != registry.StatusError || !strings.Contains(repo.LastError, wantErr.Error()) {
		t.Fatalf("repo = %+v, want error state naming enqueue failure", repo)
	}

	stored, getErr := reg.Get(context.Background(), repo.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored == nil || stored.Status != registry.StatusError {
		t.Fatalf("stored repo = %+v, want error state", stored)
	}
}

func TestSettingsSaveReportsReindexEnqueueFailure(t *testing.T) {
	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	settingsStore, err := settings.New(db, settings.Settings{})
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("task store unavailable")
	m := &Manager{
		queue:    queue.New(context.Background(), 1),
		docs:     &docsBundle{},
		settings: settingsStore,
	}
	t.Cleanup(m.queue.Close)
	bootstrapFailingTaskStore(t, m, wantErr)

	_, err = m.SetDocsConfig(context.Background(), settings.Settings{})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "settings saved but reindex enqueue failed") {
		t.Fatalf("SetDocsConfig error = %v, want partial-success reindex enqueue error", err)
	}
	if _, getErr := settingsStore.Get(context.Background()); getErr != nil {
		t.Fatalf("settings were not durably readable after enqueue failure: %v", getErr)
	}
}

func TestReindexAllAggregatesEverySubmissionFailure(t *testing.T) {
	reg := newReindexRegistry(t)
	for _, id := range []string{"acme/first", "acme/second"} {
		if err := reg.Upsert(context.Background(), &registry.Repo{ID: id, Status: registry.StatusReady}); err != nil {
			t.Fatal(err)
		}
	}

	wantErr := errors.New("task store unavailable")
	m := &Manager{reg: reg, queue: queue.New(context.Background(), 1)}
	t.Cleanup(m.queue.Close)
	store := bootstrapFailingTaskStore(t, m, wantErr)

	err := m.reindexAll(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("reindexAll error = %v, want wrapped %v", err, wantErr)
	}
	for _, id := range []string{"acme/first", "acme/second"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("reindexAll error %q does not identify failed submission %s", err, id)
		}
	}
	_, _, requests := store.snapshot()
	if len(requests) != 2 {
		t.Fatalf("reindex Save attempts = %d, want 2", len(requests))
	}
}
