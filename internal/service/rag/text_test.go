package rag

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func newTestTextStore(t *testing.T) *TextStore {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return store
}

func TestTextStoreSearchExactTermsAndFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)

	jiraDir := writeDocs(t, map[string]string{
		"pay-1842.md": "# PAY-1842 Checkout timeout\n\nThe payment gateway returns ERR_CONNECTION_RESET during capture.",
		"pay-2000.md": "# PAY-2000 Receipt copy\n\nCustomers need a second receipt.",
	})
	repoDir := writeDocs(t, map[string]string{
		"runbook.md": "# Checkout runbook\n\nRestart the payment worker after a timeout.",
	})

	if err := store.Index(ctx, "web:jira", jiraDir); err != nil {
		t.Fatal(err)
	}
	if err := store.Index(ctx, "acme/payments", repoDir); err != nil {
		t.Fatal(err)
	}

	docs, err := store.Search(ctx, vectorstore.FilterKey("web:jira"), "PAY-1842", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "pay-1842.md" {
		t.Fatalf("jira key search = %#v", docs)
	}

	docs, err = store.Search(ctx, vectorstore.Filter{Prefix: "web:"}, "ERR_CONNECTION_RESET", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Repo != "web:jira" {
		t.Fatalf("web-source filter search = %#v", docs)
	}

	docs, err = store.Search(ctx, vectorstore.Filter{ExcludePrefix: "web:"}, "checkout", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Repo != "acme/payments" {
		t.Fatalf("repo-only filter search = %#v", docs)
	}
}

func TestTextStoreIndexPathsUpdatesAndRemoves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestTextStore(t)
	dir := writeDocs(t, map[string]string{
		"keep.md": "# Keep\n\nlegacy phrase",
		"drop.md": "# Drop\n\nobsolete ticket",
	})

	if err := store.Index(ctx, "web:wiki", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.md"), []byte("# Keep\n\ncurrent phrase"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "drop.md")); err != nil {
		t.Fatal(err)
	}

	updated := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	err := store.IndexPaths(ctx, "web:wiki", dir, []string{"keep.md"}, []string{"drop.md"}, &IndexOptions{
		UpdatedAt: func(string) time.Time { return updated },
	})
	if err != nil {
		t.Fatal(err)
	}

	oldDocs, err := store.Search(ctx, vectorstore.FilterKey("web:wiki"), "legacy", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldDocs) != 0 {
		t.Fatalf("stale changed text remains: %#v", oldDocs)
	}
	dropped, err := store.Search(ctx, vectorstore.FilterKey("web:wiki"), "obsolete", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("removed path remains: %#v", dropped)
	}
	current, err := store.Search(ctx, vectorstore.FilterKey("web:wiki"), "current", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || !current[0].UpdatedAt.Equal(updated) {
		t.Fatalf("updated path = %#v", current)
	}
}
