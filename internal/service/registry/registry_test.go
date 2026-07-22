package registry

import (
	"context"
	"testing"

	"github.com/rakunlabs/bw"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatalf("open bw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := New(db)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	return reg
}

func seed(t *testing.T, reg *Registry, ids ...string) {
	t.Helper()

	for _, id := range ids {
		if err := reg.Upsert(context.Background(), &Repo{ID: id, URL: "https://example.com/" + id, Status: StatusReady}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
}

func TestListPagedOrderAndTotal(t *testing.T) {
	reg := newTestRegistry(t)
	seed(t, reg, "acme/zeta", "acme/alpha", "beta/one", "beta/two", "gamma/x")

	ctx := context.Background()

	repos, total, err := reg.ListPaged(ctx, ListOptions{Page: 1, PerPage: 2})
	if err != nil {
		t.Fatalf("list paged: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	if len(repos) != 2 {
		t.Fatalf("page size = %d, want 2", len(repos))
	}
	// Sorted by id ascending.
	if repos[0].ID != "acme/alpha" || repos[1].ID != "acme/zeta" {
		t.Fatalf("page 1 order = %q,%q", repos[0].ID, repos[1].ID)
	}

	page3, _, err := reg.ListPaged(ctx, ListOptions{Page: 3, PerPage: 2})
	if err != nil {
		t.Fatalf("list paged p3: %v", err)
	}
	if len(page3) != 1 || page3[0].ID != "gamma/x" {
		t.Fatalf("page 3 = %+v", page3)
	}
}

func TestListPagedSearch(t *testing.T) {
	reg := newTestRegistry(t)
	seed(t, reg, "acme/alpha", "acme/beta", "gamma/alphabet")

	ctx := context.Background()

	// Case-insensitive substring match on the id.
	repos, total, err := reg.ListPaged(ctx, ListOptions{Search: "ALPHA"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 2 {
		t.Fatalf("search total = %d, want 2", total)
	}
	got := map[string]bool{}
	for _, r := range repos {
		got[r.ID] = true
	}
	if !got["acme/alpha"] || !got["gamma/alphabet"] {
		t.Fatalf("search results = %+v", got)
	}
}

func TestListPagedOwnerFilter(t *testing.T) {
	reg := newTestRegistry(t)
	seed(t, reg, "acme/alpha", "acme/beta", "gamma/x")

	repos, total, err := reg.ListPaged(context.Background(), ListOptions{Owner: "acme"})
	if err != nil {
		t.Fatalf("owner filter: %v", err)
	}
	if total != 2 {
		t.Fatalf("owner total = %d, want 2", total)
	}
	for _, r := range repos {
		if r.ID[:5] != "acme/" {
			t.Fatalf("unexpected repo %q for owner acme", r.ID)
		}
	}
}

func TestOwners(t *testing.T) {
	reg := newTestRegistry(t)
	seed(t, reg, "acme/alpha", "acme/beta", "gamma/x", "solo")

	owners, err := reg.Owners(context.Background())
	if err != nil {
		t.Fatalf("owners: %v", err)
	}

	want := map[string]int{"": 1, "acme": 2, "gamma": 1}
	if len(owners) != len(want) {
		t.Fatalf("owners = %+v, want %d groups", owners, len(want))
	}
	for _, g := range owners {
		if want[g.Owner] != g.Count {
			t.Fatalf("owner %q count = %d, want %d", g.Owner, g.Count, want[g.Owner])
		}
	}
	// Sorted ascending: "" then "acme" then "gamma".
	if owners[0].Owner != "" || owners[1].Owner != "acme" || owners[2].Owner != "gamma" {
		t.Fatalf("owners not sorted: %+v", owners)
	}
}
