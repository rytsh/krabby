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

func TestListPagedStatusFilter(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()

	upsert := func(id, status string) {
		if err := reg.Upsert(ctx, &Repo{ID: id, URL: "https://example.com/" + id, Status: status}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	upsert("acme/alpha", StatusReady)
	upsert("acme/beta", StatusReady)
	upsert("acme/gamma", StatusError)
	upsert("acme/delta", StatusBuilding)

	repos, total, err := reg.ListPaged(ctx, ListOptions{Status: StatusReady})
	if err != nil {
		t.Fatalf("status filter: %v", err)
	}
	if total != 2 {
		t.Fatalf("ready total = %d, want 2", total)
	}
	for _, r := range repos {
		if r.Status != StatusReady {
			t.Fatalf("unexpected status %q for repo %q", r.Status, r.ID)
		}
	}

	// Status combines with search: only ready repos also matching "alpha".
	_, total, err = reg.ListPaged(ctx, ListOptions{Status: StatusReady, Search: "alpha"})
	if err != nil {
		t.Fatalf("status+search filter: %v", err)
	}
	if total != 1 {
		t.Fatalf("ready+alpha total = %d, want 1", total)
	}

	// Empty status returns everything.
	_, total, err = reg.ListPaged(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("no filter: %v", err)
	}
	if total != 4 {
		t.Fatalf("unfiltered total = %d, want 4", total)
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

func TestSetOverridesReportsChange(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()

	if err := reg.Upsert(ctx, &Repo{ID: "acme/app", URL: "https://git/acme/app"}); err != nil {
		t.Fatal(err)
	}

	over := Overrides{IncludeExtra: []string{"**/*.yaml"}, DocsPromptExtra: "table per env"}

	repo, prev, err := reg.SetOverrides(ctx, "acme/app", over)
	if err != nil {
		t.Fatal(err)
	}
	if !prev.Changed(repo.Overrides) {
		t.Fatal("first write must report a change so the repo is rebuilt")
	}
	if len(repo.Overrides.IncludeExtra) != 1 {
		t.Fatalf("overrides not stored: %+v", repo.Overrides)
	}

	// Re-saving the same form must not cost a rebuild.
	repo, prev, err = reg.SetOverrides(ctx, "acme/app", over)
	if err != nil {
		t.Fatal(err)
	}
	if prev.Changed(repo.Overrides) {
		t.Fatal("identical write must be a no-op")
	}

	// Blank entries from an empty text box normalize away rather than being
	// stored as a one-element list of "".
	repo, prev, err = reg.SetOverrides(ctx, "acme/app", Overrides{
		IncludeExtra: []string{"  **/*.yaml  "}, DocsPromptExtra: " table per env ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prev.Changed(repo.Overrides) {
		t.Fatal("whitespace-only differences must not trigger a rebuild")
	}

	repo, prev, err = reg.SetOverrides(ctx, "acme/app", Overrides{Include: []string{"", "   "}})
	if err != nil {
		t.Fatal(err)
	}
	if !prev.Changed(repo.Overrides) || !repo.Overrides.Empty() {
		t.Fatalf("blank globs should clear the overrides, got %+v", repo.Overrides)
	}
}

// Only a graph-exclude change invalidates the knowledge graph; the rest of the
// override set does not, and rebuilding it for them would be the most expensive
// stage of the pipeline run for nothing.
func TestOverridesGraphChanged(t *testing.T) {
	base := Overrides{GraphExclude: []string{"gen/"}, IncludeExtra: []string{"**/*.yaml"}}

	if base.GraphChanged(Overrides{GraphExclude: []string{"gen/"}, DocsPromptExtra: "x"}) {
		t.Fatal("a docs-only change must not invalidate the graph")
	}
	if !base.GraphChanged(Overrides{GraphExclude: []string{"gen/", "proto/"}}) {
		t.Fatal("a new graph exclude must invalidate the graph")
	}
	if !base.GraphChanged(Overrides{}) {
		t.Fatal("clearing the graph excludes must invalidate the graph")
	}
}
