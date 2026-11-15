package searchutil

import "sync"

// StatsCache memoises a persisted frequent-term set. Search reads it on every
// query, while it only changes when an index is rebuilt.
//
// A query that misses the cache reads the stored set and fills the cache with
// it. A refresh can store and publish a newer set in between; the generation
// keeps the query's older read from being written over it, where it would
// stick until the next refresh.
type StatsCache struct {
	mu         sync.RWMutex
	terms      StopWords
	loaded     bool
	generation uint64
}

// Get returns the cached set and whether one is loaded. When none is, gen
// identifies this miss for Fill.
func (c *StatsCache) Get() (terms StopWords, ok bool, gen uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.terms, c.loaded, c.generation
}

// Fill caches terms read after the miss Get reported as gen, unless the set
// has been published or invalidated since.
func (c *StatsCache) Fill(gen uint64, terms StopWords) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.generation != gen || c.loaded {
		return
	}
	c.terms, c.loaded = terms, true
}

// Publish caches a freshly computed set.
func (c *StatsCache) Publish(terms StopWords) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.generation++
	c.terms, c.loaded = terms, true
}

// Invalidate drops the cached set; the next query reads the stored one.
func (c *StatsCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.generation++
	c.terms, c.loaded = nil, false
}
