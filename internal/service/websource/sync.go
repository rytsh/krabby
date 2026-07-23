package websource

import "time"

// DefaultFullResyncEvery is how often an incremental provider (JIRA,
// Confluence) forces a full, non-incremental pass so remotely-deleted items are
// reconciled (pruned). Incremental "updated/lastmodified >= watermark" queries
// never return deletions, so a periodic full sweep is required.
const DefaultFullResyncEvery = 24 * time.Hour

// FullResyncDue reports whether a full pass should run now. lastFull is the time
// of the last full pass (zero on first run); every is the provider's configured
// interval (<= 0 uses DefaultFullResyncEvery). A zero lastFull always forces a
// full pass, so the first sync of a source is always complete.
func FullResyncDue(lastFull time.Time, every time.Duration) bool {
	if lastFull.IsZero() {
		return true
	}
	if every <= 0 {
		every = DefaultFullResyncEvery
	}

	return time.Since(lastFull) >= every
}
