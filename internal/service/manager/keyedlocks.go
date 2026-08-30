package manager

import "sync"

// keyedLocks hands out one mutex per key and reclaims it once nothing holds or
// waits for it.
//
// The reclamation is the point. The previous implementation kept a
// map[string]*sync.Mutex and never deleted from it, so the map grew by one
// entry for every key ever seen — every repository id, every "web:<name>" and
// "api:<name>" scope, and a "#record" variant of each — and entries for deleted
// entities were retained for the lifetime of the process.
//
// A mutex cannot simply be deleted when it is unlocked: a goroutine that is
// blocked in Lock, or that has already been handed the pointer and has not
// locked yet, still needs the entry the next caller will look up, or the two
// end up on different mutexes for the same key and the lock stops excluding
// anything. Entries are therefore reference counted, with the count taken
// before the map lock is released and dropped only after the key's mutex has
// been released.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu sync.Mutex
	// refs counts holders and waiters, guarded by the owning keyedLocks.mu.
	refs int
}

// reserve returns the entry for key with its reference count incremented.
func (k *keyedLocks) reserve(key string) *keyedLock {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}

	e, ok := k.locks[key]
	if !ok {
		e = &keyedLock{}
		k.locks[key] = e
	}
	e.refs++

	return e
}

// drop releases a reservation, removing the entry once nobody is using it.
func (k *keyedLocks) drop(key string, e *keyedLock) {
	k.mu.Lock()
	defer k.mu.Unlock()

	e.refs--
	if e.refs <= 0 {
		delete(k.locks, key)
	}
}

// releaser wraps the unlock so it runs exactly once. Releasing twice would
// otherwise unlock an unlocked mutex, which in Go is a fatal error no recover
// can contain, and would corrupt the reference count on top of it.
func (k *keyedLocks) releaser(key string, e *keyedLock) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			e.mu.Unlock()
			k.drop(key, e)
		})
	}
}

// acquire blocks until key's lock is held and returns its release function.
func (k *keyedLocks) acquire(key string) func() {
	e := k.reserve(key)
	e.mu.Lock()

	return k.releaser(key, e)
}

// tryAcquire takes key's lock if it is free. It reports whether the lock was
// taken; the release function is nil when it was not.
func (k *keyedLocks) tryAcquire(key string) (func(), bool) {
	e := k.reserve(key)
	if !e.mu.TryLock() {
		k.drop(key, e)

		return nil, false
	}

	return k.releaser(key, e), true
}

// size reports how many keys are currently reserved. It exists for tests that
// assert the map does not grow without bound.
func (k *keyedLocks) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()

	return len(k.locks)
}
