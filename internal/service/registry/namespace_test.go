package registry

import (
	"context"
	"testing"
)

func seedNamespaced(t *testing.T, reg *Registry, id, namespace string) {
	t.Helper()

	repo := &Repo{ID: id, URL: "https://example.com/" + id, Status: StatusReady, Namespace: namespace}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func TestNormalizeNamespace(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"  ":       "",
		"default":  "",
		"Default":  "",
		" TeamA ":  "teama",
		"Payments": "payments",
		"*":        "*",
	}
	for in, want := range cases {
		if got := NormalizeNamespace(in); got != want {
			t.Errorf("NormalizeNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListPagedNamespaceFilter(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()

	seedNamespaced(t, reg, "acme/untagged", "")
	seedNamespaced(t, reg, "acme/also-default", "default") // normalizes to ""
	seedNamespaced(t, reg, "acme/pay-a", "payments")
	seedNamespaced(t, reg, "acme/pay-b", "payments")
	seedNamespaced(t, reg, "acme/exp", "experiments")

	// Fix the explicit-"default" repo to store the normalized empty form the way
	// SetNamespace would; Upsert above kept it verbatim, so normalize here.
	if _, err := reg.SetNamespace(ctx, "acme/also-default", "default"); err != nil {
		t.Fatalf("set default: %v", err)
	}

	// Omitted namespace => default bucket only (the two default repos).
	_, total, err := reg.ListPaged(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("list default: %v", err)
	}
	if total != 2 {
		t.Fatalf("default namespace total = %d, want 2", total)
	}

	// Explicit "default" behaves the same.
	_, total, err = reg.ListPaged(ctx, ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatalf("list explicit default: %v", err)
	}
	if total != 2 {
		t.Fatalf("explicit default total = %d, want 2", total)
	}

	// A named namespace.
	_, total, err = reg.ListPaged(ctx, ListOptions{Namespace: "payments"})
	if err != nil {
		t.Fatalf("list payments: %v", err)
	}
	if total != 2 {
		t.Fatalf("payments total = %d, want 2", total)
	}

	// Wildcard => everything.
	_, total, err = reg.ListPaged(ctx, ListOptions{Namespace: NamespaceAll})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != 5 {
		t.Fatalf("all total = %d, want 5", total)
	}
}

func TestNamespacesGrouping(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()

	seedNamespaced(t, reg, "acme/a", "")
	seedNamespaced(t, reg, "acme/b", "payments")
	seedNamespaced(t, reg, "acme/c", "payments")

	groups, err := reg.Namespaces(ctx)
	if err != nil {
		t.Fatalf("namespaces: %v", err)
	}

	got := map[string]int{}
	for _, g := range groups {
		got[g.Namespace] = g.Count
	}
	if got[NamespaceDefault] != 1 {
		t.Errorf("default count = %d, want 1", got[NamespaceDefault])
	}
	if got["payments"] != 2 {
		t.Errorf("payments count = %d, want 2", got["payments"])
	}
}

func TestNamespaceDescriptionCRUD(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()

	// A described-but-empty namespace shows up with count 0.
	if _, err := reg.UpsertNamespace(ctx, "Payments", "payment services"); err != nil {
		t.Fatalf("upsert namespace: %v", err)
	}

	groups, err := reg.Namespaces(ctx)
	if err != nil {
		t.Fatalf("namespaces: %v", err)
	}
	var found *NamespaceGroup
	for i := range groups {
		if groups[i].Namespace == "payments" {
			found = &groups[i]
		}
	}
	if found == nil {
		t.Fatal("described namespace missing from listing")
	}
	if found.Count != 0 || found.Description != "payment services" {
		t.Fatalf("group = %+v, want count 0 + description", *found)
	}

	// Adding a repo bumps the count while keeping the description.
	seedNamespaced(t, reg, "a/pay", "payments")
	groups, _ = reg.Namespaces(ctx)
	for _, g := range groups {
		if g.Namespace == "payments" && (g.Count != 1 || g.Description != "payment services") {
			t.Fatalf("after repo: %+v", g)
		}
	}

	// Update preserves CreatedAt.
	first, _ := reg.GetNamespace(ctx, "payments")
	updated, err := reg.UpsertNamespace(ctx, "payments", "updated")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !updated.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt changed on update: %v -> %v", first.CreatedAt, updated.CreatedAt)
	}

	// Delete removes the record but the repo keeps its tag.
	if err := reg.DeleteNamespace(ctx, "payments"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rec, _ := reg.GetNamespace(ctx, "payments"); rec != nil {
		t.Fatal("namespace record not deleted")
	}
	groups, _ = reg.Namespaces(ctx)
	var stillThere bool
	for _, g := range groups {
		if g.Namespace == "payments" {
			stillThere = true
			if g.Description != "" {
				t.Fatalf("deleted namespace kept description: %+v", g)
			}
		}
	}
	if !stillThere {
		t.Fatal("namespace with repos should still list after description delete")
	}

	// The wildcard cannot be created.
	if _, err := reg.UpsertNamespace(ctx, NamespaceAll, "x"); err == nil {
		t.Fatal("expected error creating reserved namespace")
	}
}

func TestSetNamespace(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()

	seedNamespaced(t, reg, "acme/a", "")

	repo, err := reg.SetNamespace(ctx, "acme/a", "Payments")
	if err != nil {
		t.Fatalf("set namespace: %v", err)
	}
	if repo.Namespace != "payments" {
		t.Fatalf("namespace = %q, want payments", repo.Namespace)
	}

	// Moving back to default stores the empty form.
	repo, err = reg.SetNamespace(ctx, "acme/a", "default")
	if err != nil {
		t.Fatalf("reset namespace: %v", err)
	}
	if repo.Namespace != "" {
		t.Fatalf("namespace = %q, want empty", repo.Namespace)
	}

	// The wildcard cannot be assigned.
	if _, err := reg.SetNamespace(ctx, "acme/a", NamespaceAll); err == nil {
		t.Fatal("expected error assigning reserved namespace")
	}
}
