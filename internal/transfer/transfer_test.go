package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/rakunlabs/bw"
	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/taskstore"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/storage"
)

const repoID = "git.example/team/project"

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func putFile(t *testing.T, path, text string) {
	t.Helper()
	check(t, os.MkdirAll(filepath.Dir(path), 0o700))
	check(t, os.WriteFile(path, []byte(text), 0o600))
}

// Uses real registry, FTS and HNSW data: an archive that only copied application
// records would pass ordinary CRUD tests but fail these searches after import.
func seed(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	clone := filepath.Join(dir, "repos", ".snapshots", repoID, "v1")
	putFile(t, filepath.Join(clone, "main.go"), "package example")
	putFile(t, filepath.Join(clone, "graphify-out", "graph.json"), `{"nodes":[],"links":[]}`)
	putFile(t, filepath.Join(clone, "old.go"), "obsolete")
	check(t, os.Symlink("main.go", filepath.Join(clone, "alias.go")))
	docs := filepath.Join(dir, "docs", repoID)
	putFile(t, filepath.Join(docs, "doc.md"), "# Payments\nTransactional invoicing architecture")
	db, err := storage.Open(filepath.Join(dir, "state"))
	check(t, err)
	reg, err := registry.New(db)
	check(t, err)
	check(t, reg.Upsert(ctx, &registry.Repo{ID: repoID, Path: clone, Status: registry.StatusReady}))
	text, err := rag.NewTextStore(db)
	check(t, err)
	check(t, text.Index(ctx, repoID, docs))
	_, err = coderag.NewTextStore(db)
	check(t, err)
	_, err = credentials.New(db, filepath.Join(dir, "keys"))
	check(t, err)
	_, err = taskstore.New(db)
	check(t, err)
	_, err = settings.New(db, settings.Defaults())
	check(t, err)
	_, err = websource.New(db)
	check(t, err)
	_, err = apicatalog.New(db)
	check(t, err)
	check(t, db.Close())
	for _, name := range databases[1:] {
		s, err := vectorstore.New(filepath.Join(dir, name))
		check(t, err)
		check(t, s.Upsert(ctx, []vectorstore.Item{
			{ID: "keep", Vector: []float32{1, 0, 0}, Payload: vectorstore.Payload{Repo: repoID, DocPath: "doc.md", Chunk: "invoicing"}},
			{ID: "remove", Vector: []float32{0, 1, 0}, Payload: vectorstore.Payload{Repo: "removed", DocPath: "old.md", Chunk: "obsolete"}},
		}))
		check(t, s.Close())
	}
}

func assertReadable(t *testing.T, dir string, changed bool) {
	t.Helper()
	ctx := context.Background()
	a, err := Active(dir)
	check(t, err)
	if a == nil {
		t.Fatal("no active dataset")
	}
	db, err := storage.OpenReadOnly(filepath.Join(a.Directory, "state"))
	check(t, err)
	defer db.Close()
	reg, err := registry.New(db)
	check(t, err)
	reg.Relocate(a.Manifest.SourceRoot, a.Directory)
	repo, err := reg.Get(ctx, repoID)
	check(t, err)
	content, err := os.ReadFile(filepath.Join(repo.Path, "alias.go"))
	check(t, err)
	if string(content) != "package example" {
		t.Fatalf("relocated source = %s", content)
	}
	if changed {
		if _, err := os.Stat(filepath.Join(repo.Path, "old.go")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("deleted file survived")
		}
	}
	if err := reg.Upsert(ctx, repo); !errors.Is(err, badger.ErrReadOnlyTxn) {
		t.Fatalf("read-only write = %v", err)
	}
	text, err := rag.NewTextStore(db)
	check(t, err)
	hits, err := text.Search(ctx, vectorstore.FilterKey(repoID), "invoicing", 10)
	check(t, err)
	if len(hits) == 0 {
		t.Fatal("FTS index was not transferred")
	}
	for _, name := range databases[1:] {
		s, err := vectorstore.NewReadOnly(filepath.Join(a.Directory, name))
		check(t, err)
		matches, err := s.Search(ctx, vectorstore.FilterKey(repoID), []float32{1, 0, 0}, 3)
		check(t, err)
		if len(matches) != 1 || matches[0].Payload.Chunk != "invoicing" {
			t.Fatalf("vector search: %+v", matches)
		}
		has, err := s.HasRepo(ctx, "removed")
		check(t, err)
		if has == changed {
			t.Fatalf("removed vectors present=%v changed=%v", has, changed)
		}
		check(t, s.Close())
	}
}

func TestFullThenIncrementalRoundTrip(t *testing.T) {
	ctx := context.Background()
	source, target, out := t.TempDir(), t.TempDir(), t.TempDir()
	seed(t, source)
	full := filepath.Join(out, "full.krabby")
	v1, err := Export(ctx, source, full, "test", nil)
	check(t, err)
	_, err = Import(ctx, target, full, "test")
	check(t, err)
	assertReadable(t, target, false)
	_, err = Import(ctx, target, full, "test")
	check(t, err) // safe retry
	check(t, os.Remove(filepath.Join(source, "repos", ".snapshots", repoID, "v1", "old.go")))
	putFile(t, filepath.Join(source, "sources", "wiki", "new.md"), "new page")
	for _, name := range databases[1:] {
		s, err := vectorstore.New(filepath.Join(source, name))
		check(t, err)
		check(t, s.DeleteRepo(ctx, "removed"))
		check(t, s.Close())
	}
	delta := filepath.Join(out, "delta.krabby")
	v2, err := Export(ctx, source, delta, "test", &v1)
	check(t, err)
	var checkpoint Manifest
	check(t, readJSON(filepath.Join(source, ".transfer", "checkpoints", v2.ID, "manifest.json"), &checkpoint))
	if checkpoint.Databases["state"].Mode != "delta" {
		t.Fatal("unchanged state should use bw delta")
	}
	_, err = Import(ctx, target, delta, "test")
	check(t, err)
	assertReadable(t, target, true)
	a, err := Active(target)
	check(t, err)
	if a.Manifest.ID != v2.ID {
		t.Fatal("cursor did not advance")
	}
	_, err = os.ReadFile(filepath.Join(a.Directory, "sources", "wiki", "new.md"))
	check(t, err)
	// A target on no base must not partially install a delta.
	other := t.TempDir()
	if _, err := Import(ctx, other, delta, "test"); err == nil {
		t.Fatal("accepted delta without base")
	}
	if a, err := Active(other); err != nil || a != nil {
		t.Fatal("failed import activated a dataset")
	}
	// The earlier generation remains queryable after activation.
	_, err = os.Stat(filepath.Join(target, ".replica", v1.ID, "state", "MANIFEST"))
	check(t, err)
}

func TestLostHistoryFallsBackToFull(t *testing.T) {
	ctx := context.Background()
	source, target, out := t.TempDir(), t.TempDir(), t.TempDir()
	seed(t, source)
	full := filepath.Join(out, "full")
	v1, err := Export(ctx, source, full, "test", nil)
	check(t, err)
	_, err = Import(ctx, target, full, "test")
	check(t, err)
	// DropPrefix physically removes keys with no tombstones. This models the
	// lost-history case caused by migrations, wiped indexes, or compaction.
	db, err := storage.Open(filepath.Join(source, "state"))
	check(t, err)
	check(t, db.Badger().Update(func(tx *badger.Txn) error { return tx.Set([]byte("temporary-key"), []byte("temporary")) }))
	check(t, db.Close())
	basePath := filepath.Join(out, "base")
	vBase, err := Export(ctx, source, basePath, "test", nil)
	check(t, err)
	_, err = Import(ctx, target, basePath, "test")
	check(t, err)
	db, err = storage.Open(filepath.Join(source, "state"))
	check(t, err)
	check(t, db.Badger().DropPrefix([]byte("temporary-key")))
	check(t, db.Close())
	delta := filepath.Join(out, "delta")
	v2, err := Export(ctx, source, delta, "test", &vBase)
	check(t, err)
	var m Manifest
	check(t, readJSON(filepath.Join(source, ".transfer", "checkpoints", v2.ID, "manifest.json"), &m))
	if m.Databases["state"].Mode != "full" {
		t.Fatal("lost tombstone must force full database")
	}
	_, err = Import(ctx, target, delta, "test")
	check(t, err)
	assertReadable(t, target, false)
	// A cursor from another publisher is not interchangeable with this one.
	v1.DatasetID = "00000000-0000-0000-0000-000000000000"
	if _, err := Export(ctx, source, filepath.Join(out, "wrong"), "test", &v1); err == nil {
		t.Fatal("accepted foreign cursor")
	}
}

func TestImportFailurePreservesActive(t *testing.T) {
	ctx := context.Background()
	source, target, out := t.TempDir(), t.TempDir(), t.TempDir()
	seed(t, source)
	full := filepath.Join(out, "full")
	v1, err := Export(ctx, source, full, "test", nil)
	check(t, err)
	_, err = Import(ctx, target, full, "test")
	check(t, err)
	data, err := os.ReadFile(full)
	check(t, err)
	putFile(t, filepath.Join(out, "truncated"), string(data[:len(data)-8]))
	if _, err := Import(ctx, target, filepath.Join(out, "truncated"), "test"); err == nil {
		t.Fatal("accepted truncated gzip")
	}
	if _, err := Import(ctx, target, full, "other-version"); err == nil {
		t.Fatal("accepted incompatible version")
	}
	a, err := Active(target)
	check(t, err)
	if a.Manifest.ID != v1.ID {
		t.Fatal("failed import changed active snapshot")
	}
	assertReadable(t, target, false)
}

func TestLockAndReadOnlyStorage(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	check(t, err)
	if extra, err := Lock(dir); err == nil {
		extra()
		t.Fatal("concurrent transfer allowed")
	}
	unlock()
	unlock, err = Lock(dir)
	check(t, err)
	unlock()
	db, err := storage.Open(filepath.Join(dir, "db"))
	check(t, err)
	type record struct {
		ID string `bw:"id,pk"`
	}
	b, err := bw.RegisterBucket[record](db, "test")
	check(t, err)
	check(t, b.Insert(context.Background(), &record{ID: "one"}))
	check(t, db.Close())
	db, err = storage.OpenReadOnly(filepath.Join(dir, "db"))
	check(t, err)
	defer db.Close()
	b, err = bw.RegisterBucket[record](db, "test")
	check(t, err)
	if err := b.Insert(context.Background(), &record{ID: "two"}); !errors.Is(err, badger.ErrReadOnlyTxn) {
		t.Fatalf("write: %v", err)
	}
}
