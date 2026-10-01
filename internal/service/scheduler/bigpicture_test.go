package scheduler

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/storage"
)

func TestBigPictureCronReconcilesSavedSchedules(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pictures, err := bigpicture.New(db, filepath.Join(t.TempDir(), "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := bigpicture.Config{Name: "commerce", Title: "Commerce", Prompt: "Explain deployment", Sources: []bigpicture.Source{{Kind: "repo", Ref: "service"}}, Schedule: &[]string{"@every 6h"}}
	if _, err := pictures.Save(ctx, "", cfg); err != nil {
		t.Fatal(err)
	}
	mgr := &manager.Manager{}
	mgr.SetBigPictures(pictures)
	s := &scheduler{mgr: mgr}
	defer s.stop()
	s.reconcilePictures(ctx)
	if s.pictureCron == nil {
		t.Fatal("schedule not loaded")
	}
	first := s.pictureCron
	s.reconcilePictures(ctx)
	if s.pictureCron != first {
		t.Fatal("unchanged schedule rebuilt")
	}
	// A scheduled trigger that cannot queue (no model configured) is recorded
	// on the workspace instead of failing silently.
	if err := mgr.TriggerScheduledBigPicture(ctx, cfg.Name); err == nil {
		t.Fatal("unconfigured scheduled generation succeeded")
	}
	p, err := pictures.Get(ctx, cfg.Name)
	if err != nil || p.LastRun == nil || p.LastRun.Status != bigpicture.RunFailed || p.LastRun.Trigger != "schedule" {
		t.Fatalf("schedule failure not recorded: %+v %v", p, err)
	}
	cfg.ExpectedVersion = 1
	cfg.Schedule = &[]string{}
	if _, err := pictures.Save(ctx, cfg.Name, cfg); err != nil {
		t.Fatal(err)
	}
	if err := mgr.TriggerScheduledBigPicture(ctx, cfg.Name); err != nil {
		t.Fatal("disabled schedule attempted to generate")
	}
	s.reconcilePictures(ctx)
	if s.pictureCron != nil {
		t.Fatal("removed schedule still running")
	}
}
