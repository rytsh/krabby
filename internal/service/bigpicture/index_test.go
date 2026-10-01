package bigpicture

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestIndexLifecycleKeepsLiveIndexAndTracksAttempts(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, err := s.Save(ctx, "", testConfig("indexed"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Publish(ctx, p.Name, Publication{ExpectedVersion: p.Version, Producer: "test", Overview: "overview.md", Documents: []Document{{Path: "overview.md", Title: "Overview", Markdown: "# Overview"}}})
	if err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, p.Name)
	first := IndexKey(p.Name, snapshot.ID) + ":AAAAAAAAAAAAAAAAAAAAAAAAAA"
	if stale, err := s.BeginIndex(ctx, p.Name, p.StorageID, snapshot.ID, first); err != nil || len(stale) != 0 {
		t.Fatalf("begin: %v %v", stale, err)
	}
	if retiredText, retiredVector, err := s.FinishIndex(ctx, p.Name, p.StorageID, snapshot.ID, first, true, true); err != nil || retiredText != "" || retiredVector != "" {
		t.Fatalf("finish: %q %q %v", retiredText, retiredVector, err)
	}

	// An interrupted second attempt stays recorded but never replaces the
	// live index, and is returned for purging by the next attempt.
	interrupted := IndexKey(p.Name, snapshot.ID) + ":BBBBBBBBBBBBBBBBBBBBBBBBBB"
	if _, err := s.BeginIndex(ctx, p.Name, p.StorageID, snapshot.ID, interrupted); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, p.Name)
	if p.TextIndex != first || p.VectorIndex != first || !slices.Contains(p.IndexKeys(), interrupted) {
		t.Fatalf("live index changed or attempt untracked: %+v", p)
	}
	next := IndexKey(p.Name, snapshot.ID) + ":CCCCCCCCCCCCCCCCCCCCCCCCCC"
	stale, err := s.BeginIndex(ctx, p.Name, p.StorageID, snapshot.ID, next)
	if err != nil || !slices.Equal(stale, []string{interrupted}) {
		t.Fatalf("interrupted attempt not reclaimed: %v %v", stale, err)
	}

	// A text-only rebuild retires only the text side of the shared key.
	retiredText, retiredVector, err := s.FinishIndex(ctx, p.Name, p.StorageID, snapshot.ID, next, true, false)
	if err != nil || retiredText != first || retiredVector != "" {
		t.Fatalf("partial retirement: %q %q %v", retiredText, retiredVector, err)
	}
	p, _ = s.Get(ctx, p.Name)
	if p.TextIndex != next || p.VectorIndex != first || !slices.Contains(p.IndexKeys(), first) {
		t.Fatalf("vector key lost: %+v", p)
	}

	// A newer publication makes in-flight work conflict instead of going live.
	if _, err := s.Publish(ctx, p.Name, Publication{ExpectedVersion: p.Version, ExpectedRevision: snapshot.ID, Producer: "test", Overview: "overview.md", Documents: []Document{{Path: "overview.md", Title: "Overview", Markdown: "# New"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishIndex(ctx, p.Name, p.StorageID, snapshot.ID, next, true, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale finish err = %v", err)
	}
}

func TestRecordRunRejectsRecreatedWorkspace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, err := s.Save(ctx, "", testConfig("runs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRun(ctx, p.Name, "otherinstance", Run{Status: RunFailed}); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign instance err = %v", err)
	}
	if err := s.RecordRun(ctx, p.Name, p.StorageID, Run{Status: RunFailed, Message: string(make([]byte, 4096))}); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, p.Name)
	if p.LastRun == nil || p.LastRun.Status != RunFailed || len(p.LastRun.Message) > 1024 {
		t.Fatalf("run not recorded/bounded: %+v", p.LastRun)
	}
}
