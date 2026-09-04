package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

// globTestRepo registers a tracked repo whose clone is a plain directory tree,
// which is all the read paths need: repoCloneDirAt only checks that .git exists.
func globTestRepo(t *testing.T, files ...string) (*Manager, *registry.Repo) {
	t.Helper()

	dataDir := t.TempDir()
	clone := filepath.Join(dataDir, "clone")

	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, rel := range files {
		snapshotTestWrite(t, filepath.Join(clone, filepath.FromSlash(rel)), "x\n")
	}

	db, err := storage.Open(filepath.Join(dataDir, "state"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	repo := &registry.Repo{
		ID:     "example.com/team/repo",
		URL:    "https://example.com/team/repo.git",
		Branch: "main",
		Path:   clone,
		Status: registry.StatusReady,
	}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		reg:      reg,
		reposDir: filepath.Join(dataDir, "repos"),
		activity: map[string]map[string]struct{}{},
	}

	return m, repo
}

func TestGlobRepoFilesReturnsMatchesAndTheSnapshotItRead(t *testing.T) {
	t.Parallel()

	m, repo := globTestRepo(t, "Makefile", "db/migrations/001_init.sql", "internal/service/Makefile")

	page, err := m.GlobRepoFiles(context.Background(), repo.ID, "", "**/Makefile", 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Paths) != 2 || page.Paths[0] != "Makefile" || page.Paths[1] != "internal/service/Makefile" {
		t.Fatalf("glob paths = %v", page.Paths)
	}

	if page.Snapshot == "" {
		t.Fatal("glob must report the snapshot it read so a follow-up read stays on it")
	}

	// A reaped or fabricated token falls back to the active snapshot and says
	// which one it actually read, rather than failing the call.
	replayed, err := m.GlobRepoFiles(context.Background(), repo.ID, "reaped-token", "*.sql", 0)
	if err != nil {
		t.Fatal(err)
	}

	if replayed.Snapshot != page.Snapshot {
		t.Fatalf("replayed token returned snapshot %q, want the active %q", replayed.Snapshot, page.Snapshot)
	}

	if len(replayed.Paths) != 1 || replayed.Paths[0] != "db/migrations/001_init.sql" {
		t.Fatalf("glob paths = %v", replayed.Paths)
	}
}

// The manager pager must page by cursor and stamp the snapshot on every page,
// so a client walking a large listing stays on one commit for all of it.
func TestListRepoFilesPageAtPagesByCursorOnOneSnapshot(t *testing.T) {
	t.Parallel()

	files := make([]string, 0, 30)
	for i := range 30 {
		files = append(files, fmt.Sprintf("pkg%02d/f.go", i))
	}

	m, repo := globTestRepo(t, files...)

	seen := map[string]bool{}
	cursor := ""

	for pages := 1; ; pages++ {
		if pages > 100 {
			t.Fatal("cursor paging did not terminate")
		}

		page, err := m.ListRepoFilesPageAt(context.Background(), repo.ID, "", "", true, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}

		if page.Snapshot == "" {
			t.Fatalf("page %d is missing its snapshot token", pages)
		}

		for _, e := range page.Entries {
			if seen[e.Path] {
				t.Fatalf("entry %s served twice", e.Path)
			}

			seen[e.Path] = true
		}

		if page.NextCursor == "" {
			break
		}

		cursor = page.NextCursor
	}

	// 30 directories plus 30 files, every one reachable by paging.
	if len(seen) != 60 {
		t.Fatalf("paged listing reached %d of 60 entries", len(seen))
	}
}
