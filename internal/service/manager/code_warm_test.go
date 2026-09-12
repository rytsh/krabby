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
	"github.com/rytsh/krabby/internal/service/vectorstore"
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

// TestSearchCodeTextDoesNotBlockOnPendingCrossRepoWarm is the regression test
// for the first cross-repo search after a bulk import hanging. Warming ran for
// every repository in scope, in series, inside the request — so a query over
// 200 freshly added repos waited for all 200 clones to be chunked and indexed,
// with no server-side deadline to end it. A cross-repo search must answer from
// what is indexed now and report the rest as pending.
func TestSearchCodeTextDoesNotBlockOnPendingCrossRepoWarm(t *testing.T) {
	t.Parallel()

	const (
		ready    = "acme/ready"
		building = "acme/building"
	)

	m, ctx := codeSearchFixture(t, ready, []vectorstore.Item{
		codeChunk(ready, "gateway.go", "Capture", 10, "func Capture() error { return gateway.Timeout }"),
	})

	if err := m.reg.Upsert(ctx, &registry.Repo{ID: building, URL: "https://example.com/" + building}); err != nil {
		t.Fatal(err)
	}

	// A background warm owns the permit for the whole of its build, exactly as
	// it would mid-import.
	lk := semaphore.NewWeighted(1)
	if err := lk.Acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	defer lk.Release(1)
	m.codeWarm = map[string]*semaphore.Weighted{building: lk}

	type result struct {
		page coderag.SearchPage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		page, err := m.SearchCodeText(ctx, "", registry.NamespaceAll, "gateway",
			coderag.TextSearchOptions{PerPage: 10})
		done <- result{page, err}
	}()

	var got coderag.SearchPage
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("cross-repo search while a warm is in flight: %v", r.err)
		}
		got = r.page
	case <-time.After(5 * time.Second):
		t.Fatal("cross-repo search blocked on the in-flight warm instead of answering from the index")
	}

	if len(got.Results) == 0 {
		t.Fatal("search returned no results from the repository that is indexed")
	}

	// Not waiting is only acceptable because the page says so.
	var reported bool
	for _, idx := range got.Indexed {
		if idx.Repo == building {
			reported = true
			if !idx.Pending {
				t.Fatalf("repository still building reported as ready: %#v", idx)
			}
		}
	}
	if !reported {
		t.Fatalf("page omitted the repository it could not search: %#v", got.Indexed)
	}
}

// A search that names one repository keeps the old guarantee: the answer is
// about that repo, so a half-built index is a wrong answer, not a partial one.
func TestSearchCodeTextWaitsForNamedRepo(t *testing.T) {
	t.Parallel()

	const repoID = "acme/building"

	m, ctx := codeSearchFixture(t, repoID, nil)

	lk := semaphore.NewWeighted(1)
	if err := lk.Acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	m.codeWarm = map[string]*semaphore.Weighted{repoID: lk}

	done := make(chan error, 1)
	go func() {
		_, err := m.SearchCodeText(ctx, repoID, registry.NamespaceAll, "gateway",
			coderag.TextSearchOptions{PerPage: 10})
		done <- err
	}()

	select {
	case err := <-done:
		lk.Release(1)
		t.Fatalf("search for a named repository did not wait for its build: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	lk.Release(1)
	if err := <-done; err != nil {
		t.Fatalf("search after the build released: %v", err)
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
