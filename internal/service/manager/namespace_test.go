package manager

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

func newNamespaceManager(t *testing.T) (*Manager, *registry.Registry) {
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

	return &Manager{reg: reg}, reg
}

func TestRepoInNamespace(t *testing.T) {
	def := &registry.Repo{ID: "a/def"}
	pay := &registry.Repo{ID: "a/pay", Namespace: "payments"}

	cases := []struct {
		repo *registry.Repo
		ns   string
		want bool
	}{
		{def, "", true},
		{def, "default", true},
		{def, "payments", false},
		{def, registry.NamespaceAll, true},
		{pay, "", false},
		{pay, "default", false},
		{pay, "payments", true},
		{pay, "Payments", true}, // case-insensitive
		{pay, registry.NamespaceAll, true},
	}
	for _, tc := range cases {
		if got := repoInNamespace(tc.repo, tc.ns); got != tc.want {
			t.Errorf("repoInNamespace(%q, %q) = %t, want %t", tc.repo.ID, tc.ns, got, tc.want)
		}
	}
}

func TestSetRepoNamespaceResolvesRef(t *testing.T) {
	mgr, reg := newNamespaceManager(t)
	ctx := context.Background()

	if err := reg.Upsert(ctx, &registry.Repo{ID: "host/group/svc", Status: registry.StatusReady}); err != nil {
		t.Fatal(err)
	}

	// Resolve by suffix, not full id.
	repo, err := mgr.SetRepoNamespace(ctx, "svc", "payments")
	if err != nil {
		t.Fatalf("set namespace: %v", err)
	}
	if repo.Namespace != "payments" {
		t.Fatalf("namespace = %q, want payments", repo.Namespace)
	}

	got, err := reg.Get(ctx, "host/group/svc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "payments" {
		t.Fatalf("persisted namespace = %q, want payments", got.Namespace)
	}

	if _, err := mgr.SetRepoNamespace(ctx, "missing", "x"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestNamespaceScope(t *testing.T) {
	mgr, reg := newNamespaceManager(t)
	ctx := context.Background()

	for _, r := range []*registry.Repo{
		{ID: "a/one", Namespace: "payments"},
		{ID: "a/two", Namespace: "payments"},
		{ID: "a/def", Namespace: ""},
	} {
		if err := reg.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// Explicit repo => unrestricted.
	scope, err := mgr.namespaceScope(ctx, "a/one", "")
	if err != nil {
		t.Fatal(err)
	}
	if !scope.all {
		t.Fatal("explicit repo should yield all scope")
	}

	// Wildcard => unrestricted.
	scope, err = mgr.namespaceScope(ctx, "", registry.NamespaceAll)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.all {
		t.Fatal("wildcard should yield all scope")
	}

	// Default bucket => single repo.
	scope, err = mgr.namespaceScope(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if scope.single != "a/def" {
		t.Fatalf("default single = %q, want a/def", scope.single)
	}

	// Multi-repo namespace.
	scope, err = mgr.namespaceScope(ctx, "", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if scope.single != "" || len(scope.repos) != 2 {
		t.Fatalf("payments scope = %+v, want 2 repos", scope.repos)
	}
	if !scope.contains("a/one") || scope.contains("a/def") {
		t.Fatalf("payments scope membership wrong: %+v", scope.set)
	}

	// Empty namespace with no matches errors.
	if _, err := mgr.namespaceScope(ctx, "", "nope"); err == nil {
		t.Fatal("expected error for empty namespace")
	}
}
