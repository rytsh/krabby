package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

func TestMigrateLegacyDocsPreservesCacheOutsideClone(t *testing.T) {
	dataDir := t.TempDir()
	cloneDir := filepath.Join(dataDir, "repos", "owner", "repo")
	legacyDir := filepath.Join(cloneDir, "krabby-docs")
	docsDir := filepath.Join(dataDir, "docs", "owner", "repo")
	mustWriteManagerTest(t, filepath.Join(legacyDir, "docs-index.json"), "manifest")
	mustWriteManagerTest(t, filepath.Join(legacyDir, ".summaries", "main.go.sum"), "cached")

	if err := migrateLegacyDocs(legacyDir, docsDir); err != nil {
		t.Fatalf("migrateLegacyDocs: %v", err)
	}

	if _, err := os.Stat(legacyDir); !os.IsNotExist(err) {
		t.Fatalf("legacy docs remain in clone: %v", err)
	}
	for rel, want := range map[string]string{
		"docs-index.json":        "manifest",
		".summaries/main.go.sum": "cached",
	} {
		got, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read migrated %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("migrated %s = %q, want %q", rel, got, want)
		}
	}
}

func TestRemoveRepoRemovesExternalDocs(t *testing.T) {
	dataDir := t.TempDir()
	db, err := storage.Open(filepath.Join(dataDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	reposDir := filepath.Join(dataDir, "repos")
	cloneDir := filepath.Join(reposDir, "owner", "repo")
	docsRoot := filepath.Join(dataDir, "docs")
	docsDir := filepath.Join(docsRoot, "owner", "repo")
	mustWriteManagerTest(t, filepath.Join(cloneDir, ".git", "config"), "")
	mustWriteManagerTest(t, filepath.Join(docsDir, "documentation.md"), "# Docs")

	repo := &registry.Repo{ID: "owner/repo", Path: cloneDir, Status: registry.StatusReady}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	mgr := New(context.Background(), reg, nil, nil, graphquery.NewEngine(), nil, nil,
		reposDir, filepath.Join(dataDir, "merged", "graph.json"), DocsDeps{DocsRootDir: docsRoot})
	mgr.docs = &docsBundle{}

	if err := mgr.RemoveRepo(context.Background(), repo.ID); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	mgr.Wait()

	if _, err := os.Stat(docsDir); !os.IsNotExist(err) {
		t.Fatalf("external docs still exist: %v", err)
	}
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Fatalf("clone still exists: %v", err)
	}
}

func TestRepoDocsPathRejectsTraversal(t *testing.T) {
	mgr := &Manager{docsRootDir: t.TempDir()}
	for _, id := range []string{"../outside", "owner/../../outside", "/outside", "owner//repo"} {
		if _, _, err := mgr.repoDocsPath(id); err == nil {
			t.Fatalf("repoDocsPath(%q) accepted unsafe id", id)
		}
	}
}

func TestListDocsNormalizesNullManifestDocs(t *testing.T) {
	dataDir := t.TempDir()
	db, err := storage.Open(filepath.Join(dataDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	cloneDir := filepath.Join(dataDir, "repos", "owner", "repo")
	docsRoot := filepath.Join(dataDir, "docs")
	mustWriteManagerTest(t, filepath.Join(cloneDir, ".git", "config"), "")
	mustWriteManagerTest(t, filepath.Join(docsRoot, "owner", "repo", "docs-index.json"), `{"repo":"owner/repo","docs":null}`)

	repo := &registry.Repo{ID: "owner/repo", Path: cloneDir, Status: registry.StatusReady}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	mgr := New(context.Background(), reg, nil, nil, graphquery.NewEngine(), nil, nil,
		filepath.Join(dataDir, "repos"), filepath.Join(dataDir, "merged", "graph.json"), DocsDeps{DocsRootDir: docsRoot})
	docs, err := mgr.ListDocs(context.Background(), repo.ID)
	if err != nil {
		t.Fatalf("ListDocs: %v", err)
	}
	if docs == nil || len(docs) != 0 {
		t.Fatalf("ListDocs() = %#v, want non-nil empty slice", docs)
	}
}

func mustWriteManagerTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
