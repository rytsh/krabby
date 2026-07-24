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
	s.Save(1, queue.Spec{Kind: "refresh", ID: "acme/repo"}, now)
	s.Save(2, queue.Spec{
		Kind:   "generate",
		ID:     "acme/repo",
		Params: map[string]string{"targets": "graph,docs", "force": "true"},
	}, now)

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
	s.Remove(1)
	got, err = s.List(ctx)
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("after remove = %+v, want only seq 2", got)
	}

	// Removing a missing record is a no-op.
	s.Remove(999)
}

func TestSaveReplaces(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Save(1, queue.Spec{Kind: "refresh", ID: "a"}, time.Now())
	s.Save(1, queue.Spec{Kind: "refresh", ID: "b"}, time.Now())

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (replace, not append)", len(got))
	}
	if got[0].Spec.ID != "b" {
		t.Fatalf("id = %q, want b", got[0].Spec.ID)
	}
}
