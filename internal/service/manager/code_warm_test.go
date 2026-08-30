package manager

import (
	"context"
	"sync"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/registry"
)

func TestEnsureCodeIndexForSearchSkipsVanishedRepo(t *testing.T) {
	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	text, err := coderag.NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	const repoID = "owner/vanished"
	m := &Manager{
		reg:      reg,
		codeText: text,
		codeWarm: map[string]*sync.Mutex{repoID: {}},
	}
	if err := m.ensureCodeIndexForSearch(context.Background(), repoID, namespaceScope{all: true}); err != nil {
		t.Fatalf("ensureCodeIndexForSearch: %v", err)
	}
	if _, pending := m.codeWarmLock(repoID); pending {
		t.Fatal("vanished repository remained pending")
	}
}

func TestEnsureCodeIndexForSearchReturnsRegistryError(t *testing.T) {
	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.New(db)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	text, err := coderag.NewTextStore(db)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	const repoID = "owner/unreadable"
	m := &Manager{
		reg:      reg,
		codeText: text,
		codeWarm: map[string]*sync.Mutex{repoID: {}},
	}
	if err := m.ensureCodeIndexForSearch(context.Background(), repoID, namespaceScope{all: true}); err == nil {
		t.Fatal("ensureCodeIndexForSearch returned nil for a registry storage error")
	}
	if _, pending := m.codeWarmLock(repoID); !pending {
		t.Fatal("registry storage error incorrectly cleared pending warmup")
	}
}
