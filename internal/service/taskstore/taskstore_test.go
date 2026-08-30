package taskstore

import (
	"context"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/queue"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatalf("open bw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s, err := New(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	return s
}

func TestSaveListRemove(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Save(1, queue.Spec{Kind: "refresh", ID: "acme/repo"}, now); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := s.Save(2, queue.Spec{
		Kind:   "generate",
		ID:     "acme/repo",
		Params: map[string]string{"targets": "graph,docs", "force": "true"},
	}, now); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	// Ordered by seq ascending.
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("order = %d,%d, want 1,2", got[0].Seq, got[1].Seq)
	}

	if got[0].Spec.Kind != "refresh" || got[0].Spec.ID != "acme/repo" {
		t.Fatalf("spec[0] = %+v", got[0].Spec)
	}

	if got[1].Spec.Params["targets"] != "graph,docs" || got[1].Spec.Params["force"] != "true" {
		t.Fatalf("spec[1] params = %+v", got[1].Spec.Params)
	}

	// Remove one; the other survives.
	if err := s.Remove(1); err != nil {
		t.Fatalf("remove 1: %v", err)
	}
	got, err = s.List(ctx)
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("after remove = %+v, want only seq 2", got)
	}

	// Removing a missing record is a no-op.
	if err := s.Remove(999); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
}

func TestSaveRejectsSequenceCollision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Save(1, queue.Spec{Kind: "refresh", ID: "a"}, time.Now()); err != nil {
		t.Fatalf("save original: %v", err)
	}
	if err := s.Save(1, queue.Spec{Kind: "refresh", ID: "b"}, time.Now()); err == nil {
		t.Fatal("duplicate sequence save succeeded")
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Spec.ID != "a" {
		t.Fatalf("id = %q, want original a", got[0].Spec.ID)
	}
}
