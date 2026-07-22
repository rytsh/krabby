package manager

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

func TestReconcileInterruptedStages(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	repo := &registry.Repo{ID: "owner/repo", Status: registry.StatusReady}
	repo.Stages.CodeIndex.Status = registry.StageRunning
	repo.Stages.Docs.Status = registry.StageOK
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{reg: reg}
	if err := mgr.ReconcileInterruptedStages(context.Background()); err != nil {
		t.Fatalf("ReconcileInterruptedStages: %v", err)
	}

	got, err := reg.Get(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stages.CodeIndex.Status != registry.StageError {
		t.Fatalf("code index status = %q, want %q", got.Stages.CodeIndex.Status, registry.StageError)
	}
	if got.Stages.CodeIndex.Error != "interrupted by service restart" {
		t.Fatalf("code index error = %q", got.Stages.CodeIndex.Error)
	}
	if got.Stages.CodeIndex.FinishedAt.IsZero() {
		t.Fatal("code index finished time was not recorded")
	}
	if got.Stages.Docs.Status != registry.StageOK {
		t.Fatalf("completed docs stage changed to %q", got.Stages.Docs.Status)
	}
}
