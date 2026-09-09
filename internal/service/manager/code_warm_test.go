package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rakunlabs/bw"
	"golang.org/x/sync/semaphore"

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
		codeWarm: map[string]*semaphore.Weighted{repoID: semaphore.NewWeighted(1)},
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
		codeWarm: map[string]*semaphore.Weighted{repoID: semaphore.NewWeighted(1)},
	}
	if err := m.ensureCodeIndexForSearch(context.Background(), repoID, namespaceScope{all: true}); err == nil {
		t.Fatal("ensureCodeIndexForSearch returned nil for a registry storage error")
	}
	if _, pending := m.codeWarmLock(repoID); !pending {
		t.Fatal("registry storage error incorrectly cleared pending warmup")
	}
}

func TestEnsureCodeIndexWaitHonoursCancellation(t *testing.T) {
	const repoID = "owner/building"
	lk := semaphore.NewWeighted(1)
	if err := lk.Acquire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	defer lk.Release(1)
	m := &Manager{
		codeText: &coderag.TextStore{},
		codeWarm: map[string]*semaphore.Weighted{repoID: lk},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- m.ensureCodeIndex(ctx, repoID, "") }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled search is still waiting for the background build")
	}
	if _, pending := m.codeWarmLock(repoID); !pending {
		t.Fatal("cancelled waiter cleared the background build's pending state")
	}
}
