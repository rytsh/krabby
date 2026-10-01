package manager

import (
	"sync"
	"testing"
	"time"
)

func TestBundleReplacementWaitsForLease(t *testing.T) {
	oldStore := &lifecycleStore{}
	newStore := &lifecycleStore{}
	old := &docsBundle{store: oldStore}
	next := &docsBundle{store: newStore}
	state := bundleState{docs: old}
	leased, release := state.acquire()
	var once sync.Once
	releaseOnce := func() { once.Do(release) }
	defer releaseOnce()
	if leased != old {
		t.Fatal("lease did not return the active bundle")
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- state.replace(next)
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("replacement completed with an outstanding lease: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if oldStore.closed.Load() != 0 {
		t.Fatal("leased resources were closed")
	}

	releaseOnce()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not finish after lease release")
	}
	if oldStore.closed.Load() != 1 || newStore.closed.Load() != 0 {
		t.Fatal("replacement closed the wrong resources")
	}
	leased, release = state.acquire()
	if leased != next {
		t.Error("new leases did not observe the replacement")
	}
	release()
	if err := state.replace(&docsBundle{}); err != nil {
		t.Fatal(err)
	}
	if newStore.closed.Load() != 1 {
		t.Fatal("shutdown did not close the replacement")
	}
}

func TestBundleReplacementTransfersSharedStore(t *testing.T) {
	shared := &lifecycleStore{}
	state := bundleState{docs: &docsBundle{store: shared, codeStore: shared}}
	if err := state.replace(&docsBundle{codeStore: shared}); err != nil {
		t.Fatal(err)
	}
	if shared.closed.Load() != 0 {
		t.Fatal("transferred store was closed")
	}
	if err := state.replace(&docsBundle{}); err != nil {
		t.Fatal(err)
	}
	if shared.closed.Load() != 1 {
		t.Fatal("transferred store was not closed exactly once")
	}
}
