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

// A replacement waiting on a long lease must not hold back new leases, and a
// goroutine that already holds one must be able to take another. Both used to
// block on a writer-preferring RWMutex: a settings save during a docs build
// stalled every search and traced request until the build ended, and a lease
// taken twice with the save in between never returned.
func TestBundleLeasesDoNotWaitForAPendingReplacement(t *testing.T) {
	oldStore := &lifecycleStore{}
	old := &docsBundle{store: oldStore}
	next := &docsBundle{}
	state := bundleState{docs: old}

	long, releaseLong := state.acquire()
	if long != old {
		t.Fatal("lease did not return the active bundle")
	}

	replaced := make(chan error, 1)
	go func() { replaced <- state.replace(next) }()

	// Wait until the swap happened; the replacement is then waiting on the
	// long lease.
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, release := state.acquire()
		release()
		if b == next {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement never swapped the bundle")
		}
		time.Sleep(time.Millisecond)
	}

	leased := make(chan *docsBundle, 1)
	go func() {
		// The holder of the long lease taking a second one.
		b, release := state.acquire()
		release()
		leased <- b
	}()
	select {
	case b := <-leased:
		if b != next {
			t.Error("a lease taken after the swap got the old bundle")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a new lease blocked behind the pending replacement")
	}

	select {
	case err := <-replaced:
		t.Fatalf("replacement finished with a lease still out: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if oldStore.closed.Load() != 0 {
		t.Fatal("the old bundle was closed under a live lease")
	}

	releaseLong()
	releaseLong() // a second call is a no-op
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not finish after the last lease was released")
	}
	if oldStore.closed.Load() != 1 {
		t.Fatalf("old store closed %d times, want 1", oldStore.closed.Load())
	}
}
