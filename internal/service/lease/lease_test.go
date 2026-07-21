package lease

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAcquireRelease(t *testing.T) {
	m := New(nil)

	l, err := m.Acquire("owner/repo", "docgen", time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if l.Token == "" {
		t.Fatal("empty token")
	}

	if !m.Active("owner/repo") {
		t.Error("lease should be active")
	}

	// Second acquire fails.
	if _, err := m.Acquire("owner/repo", "other", time.Minute); !errors.Is(err, ErrLeased) {
		t.Errorf("second acquire err = %v, want ErrLeased", err)
	}

	// Wrong token rejected.
	if err := m.Release("owner/repo", "wrong"); !errors.Is(err, ErrBadToken) {
		t.Errorf("release wrong token err = %v, want ErrBadToken", err)
	}

	// Correct release.
	if err := m.Release("owner/repo", l.Token); err != nil {
		t.Fatalf("release: %v", err)
	}

	if m.Active("owner/repo") {
		t.Error("lease should be gone")
	}

	if err := m.Release("owner/repo", l.Token); !errors.Is(err, ErrNotLeased) {
		t.Errorf("double release err = %v, want ErrNotLeased", err)
	}
}

func TestInfoHidesToken(t *testing.T) {
	m := New(nil)

	if _, err := m.Acquire("a/b", "x", time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	info := m.Info("a/b")
	if info == nil || info.Token != "" {
		t.Errorf("Info = %+v, want lease without token", info)
	}

	if m.Info("missing/repo") != nil {
		t.Error("Info for unknown repo should be nil")
	}
}

func TestPendingFiresOnRelease(t *testing.T) {
	var (
		mu    sync.Mutex
		fired []string
	)

	m := New(func(id string) {
		mu.Lock()
		defer mu.Unlock()

		fired = append(fired, id)
	})

	l, _ := m.Acquire("a/b", "x", time.Minute)
	m.MarkPending("a/b")

	if err := m.Release("a/b", l.Token); err != nil {
		t.Fatalf("release: %v", err)
	}

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(fired) == 1 && fired[0] == "a/b"
	})
}

func TestPendingFiresOnExpiry(t *testing.T) {
	var (
		mu    sync.Mutex
		fired []string
	)

	m := New(func(id string) {
		mu.Lock()
		defer mu.Unlock()

		fired = append(fired, id)
	})

	if _, err := m.Acquire("a/b", "x", 20*time.Millisecond); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	m.MarkPending("a/b")

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(fired) == 1
	})

	if m.Active("a/b") {
		t.Error("lease should have expired")
	}
}

func TestTTLClamp(t *testing.T) {
	m := New(nil)

	l, err := m.Acquire("a/b", "", 10*time.Hour)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if until := time.Until(l.ExpiresAt); until > MaxTTL+time.Second {
		t.Errorf("ttl not capped: %s", until)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("condition not met in time")
}
