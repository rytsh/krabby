package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

type lifecycleStore struct {
	closed  atomic.Int32
	onClose func()
}

func (*lifecycleStore) Upsert(context.Context, []vectorstore.Item) error { return nil }

func (*lifecycleStore) Search(context.Context, vectorstore.Filter, []float32, int) ([]vectorstore.Match, error) {
	return nil, nil
}

func (*lifecycleStore) DeleteRepo(context.Context, string) error { return nil }
func (*lifecycleStore) HasRepo(context.Context, string) (bool, error) {
	return false, nil
}
func (*lifecycleStore) IndexedPaths(context.Context, string) (map[string]struct{}, error) {
	return nil, nil
}
func (*lifecycleStore) DeletePaths(context.Context, string, []string) error { return nil }
func (s *lifecycleStore) Close() error {
	s.closed.Add(1)
	if s.onClose != nil {
		s.onClose()
	}

	return nil
}

func TestBuildBundleRollbackClosesNewResources(t *testing.T) {
	wantErr := errors.New("open code vectors")
	docsStore := &lifecycleStore{}
	newTracer := langfuse.Disabled()
	var opens, tracerShutdowns atomic.Int32

	m := &Manager{
		docs: &docsBundle{},
		openVectorStore: func(string) (vectorstore.Store, error) {
			if opens.Add(1) == 1 {
				return docsStore, nil
			}

			return nil, wantErr
		},
		newLangfuseTracer: func(config.Langfuse) (*langfuse.Tracer, error) {
			return newTracer, nil
		},
		shutdownLangfuseTracer: func(context.Context, *langfuse.Tracer) error {
			tracerShutdowns.Add(1)

			return nil
		},
	}

	_, err := m.buildBundle(settings.Settings{
		RAGEnabled:     true,
		CodeRAGEnabled: true,
		EmbedBaseURL:   "http://embedder.test",
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("buildBundle error = %v, want %v", err, wantErr)
	}
	if got := docsStore.closed.Load(); got != 1 {
		t.Fatalf("new docs store closed %d times, want 1", got)
	}
	if got := tracerShutdowns.Load(); got != 1 {
		t.Fatalf("new tracer shut down %d times, want 1", got)
	}
}

func TestBuildBundleRollbackRetainsBorrowedTracer(t *testing.T) {
	wantErr := errors.New("open code vectors")
	docsStore := &lifecycleStore{}
	borrowedTracer := langfuse.Disabled()
	var opens, tracerShutdowns atomic.Int32

	m := &Manager{
		docs: &docsBundle{
			tracer: borrowedTracer,
			tracerShutdown: func(context.Context) error {
				tracerShutdowns.Add(1)

				return nil
			},
		},
		openVectorStore: func(string) (vectorstore.Store, error) {
			if opens.Add(1) == 1 {
				return docsStore, nil
			}

			return nil, wantErr
		},
		newLangfuseTracer: func(config.Langfuse) (*langfuse.Tracer, error) {
			t.Fatal("equivalent active tracer was rebuilt")

			return nil, nil
		},
	}

	_, err := m.buildBundle(settings.Settings{
		RAGEnabled:     true,
		CodeRAGEnabled: true,
		EmbedBaseURL:   "http://embedder.test",
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("buildBundle error = %v, want %v", err, wantErr)
	}
	if got := docsStore.closed.Load(); got != 1 {
		t.Fatalf("new docs store closed %d times, want 1", got)
	}
	if got := tracerShutdowns.Load(); got != 0 {
		t.Fatalf("borrowed tracer shut down %d times", got)
	}
}

func TestDocsBundleCloseExceptRetainsTransferredResources(t *testing.T) {
	transferredStore := &lifecycleStore{}
	replacedStore := &lifecycleStore{}
	transferredTracer := langfuse.Disabled()
	var tracerShutdowns atomic.Int32

	old := &docsBundle{
		store:     transferredStore,
		codeStore: replacedStore,
		tracer:    transferredTracer,
		tracerShutdown: func(context.Context) error {
			tracerShutdowns.Add(1)

			return nil
		},
	}
	keep := &docsBundle{codeStore: transferredStore, tracer: transferredTracer}

	if err := old.closeExcept(keep); err != nil {
		t.Fatal(err)
	}
	if got := transferredStore.closed.Load(); got != 0 {
		t.Fatalf("transferred store closed %d times", got)
	}
	if got := replacedStore.closed.Load(); got != 1 {
		t.Fatalf("replaced store closed %d times, want 1", got)
	}
	if got := tracerShutdowns.Load(); got != 0 {
		t.Fatalf("transferred tracer shut down %d times", got)
	}
}

func TestManagerCloseDrainsQueueBeforeBundle(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	store := &lifecycleStore{onClose: func() { record("store") }}
	var tracerShutdowns atomic.Int32
	m := &Manager{
		queue: queue.New(context.Background(), 1),
		docs: &docsBundle{
			store:  store,
			tracer: langfuse.Disabled(),
			tracerShutdown: func(context.Context) error {
				tracerShutdowns.Add(1)
				record("tracer")

				return nil
			},
		},
	}

	started := make(chan struct{})
	checked := make(chan bool, 1)
	m.queue.Submit(queue.Task{Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		checked <- tracerShutdowns.Load() == 0 && store.closed.Load() == 0

		return nil
	}})
	<-started

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()

	if resourcesOpen := <-checked; !resourcesOpen {
		t.Fatal("manager released bundle resources before queued work exited")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "tracer" || events[1] != "store" {
		t.Fatalf("close events = %v, want [tracer store]", events)
	}
}
