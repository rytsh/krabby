// Package lease provides TTL-bounded read leases on repositories. While a
// lease is active, krabby defers git mutations (pull/rebuild) so external
// tools can walk a clone without racing a refresh. Deferred refreshes fire
// automatically when the lease is released or expires.
package lease

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TTL bounds.
const (
	DefaultTTL = 10 * time.Minute
	MaxTTL     = time.Hour
)

// Lease errors.
var (
	ErrLeased    = errors.New("repo is already leased")
	ErrNotLeased = errors.New("repo is not leased")
	ErrBadToken  = errors.New("lease token mismatch")
)

// Lease is an active read lease on a repository.
type Lease struct {
	Repo  string `json:"repo"`
	Owner string `json:"owner,omitempty"`
	// Token authorizes release. Only returned on acquire.
	Token     string    `json:"token,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`

	timer *time.Timer
}

// Manager tracks leases per repo id.
type Manager struct {
	mu      sync.Mutex
	leases  map[string]*Lease
	pending map[string]bool
	// onFree is called (in a new goroutine) when a repo with a pending
	// refresh loses its lease.
	onFree func(id string)
}

// New creates a lease manager. onFree may be nil.
func New(onFree func(id string)) *Manager {
	return &Manager{
		leases:  map[string]*Lease{},
		pending: map[string]bool{},
		onFree:  onFree,
	}
}

// Acquire takes a lease on id. ttl <= 0 uses DefaultTTL; ttl is capped at MaxTTL.
func (m *Manager) Acquire(id, owner string, ttl time.Duration) (*Lease, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	if ttl > MaxTTL {
		ttl = MaxTTL
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.leases[id]; ok {
		return nil, fmt.Errorf("%w: by %q until %s", ErrLeased, existing.Owner, existing.ExpiresAt.Format(time.RFC3339))
	}

	l := &Lease{
		Repo:      id,
		Owner:     owner,
		Token:     token,
		ExpiresAt: time.Now().Add(ttl),
	}
	l.timer = time.AfterFunc(ttl, func() { m.expire(id, token) })

	m.leases[id] = l

	return l, nil
}

// Release ends a lease. The token must match the one returned by Acquire.
func (m *Manager) Release(id, token string) error {
	m.mu.Lock()

	l, ok := m.leases[id]
	if !ok {
		m.mu.Unlock()

		return ErrNotLeased
	}

	if subtle.ConstantTimeCompare([]byte(l.Token), []byte(token)) != 1 {
		m.mu.Unlock()

		return ErrBadToken
	}

	l.timer.Stop()
	m.free(id)
	m.mu.Unlock()

	return nil
}

// Active reports whether id currently holds a live lease.
func (m *Manager) Active(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	l, ok := m.leases[id]

	return ok && time.Now().Before(l.ExpiresAt)
}

// Info returns the lease for id without its token, or nil.
func (m *Manager) Info(id string) *Lease {
	m.mu.Lock()
	defer m.mu.Unlock()

	l, ok := m.leases[id]
	if !ok {
		return nil
	}

	return &Lease{Repo: l.Repo, Owner: l.Owner, ExpiresAt: l.ExpiresAt}
}

// MarkPending records that a refresh was deferred for id; it fires via onFree
// once the lease is released or expires.
func (m *Manager) MarkPending(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pending[id] = true
}

// expire is the timer callback: drops the lease if it is still the same one.
func (m *Manager) expire(id, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	l, ok := m.leases[id]
	if !ok || l.Token != token {
		return
	}

	m.free(id)
}

// free removes the lease and fires a pending refresh. Caller holds m.mu.
func (m *Manager) free(id string) {
	delete(m.leases, id)

	if m.pending[id] {
		delete(m.pending, id)

		if m.onFree != nil {
			go m.onFree(id)
		}
	}
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token; %w", err)
	}

	return hex.EncodeToString(b), nil
}
