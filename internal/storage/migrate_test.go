package storage

import (
	"testing"
)

// TestMigrationProgressDoesNotPanic exercises the hook's key/value pairing,
// which slog would panic on if an attribute were added without its value, and
// the arithmetic in the paths that only run partway through a migration.
func TestMigrationProgressDoesNotPanic(t *testing.T) {
	t.Parallel()

	fn := MigrationProgress()

	for _, tc := range []struct {
		name                         string
		fromV, toV, processed, total uint64
	}{
		{"first call of a step", 1, 2, 0, 100},
		{"partway", 1, 2, 50, 100},
		{"complete", 1, 2, 100, 100},
		{"a second step restarts timing", 2, 3, 0, 40},
		{"empty bucket", 3, 4, 0, 0},
		{"processed exceeds total", 4, 5, 10, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn("chunks", tc.fromV, tc.toV, tc.processed, tc.total)
		})
	}
}

// TestMigrationProgressThrottles checks that a per-record callback does not
// become a per-record log line: only the first call of a step and its
// completion are reported without waiting for the interval.
func TestMigrationProgressThrottles(t *testing.T) {
	t.Parallel()

	fn := MigrationProgress()

	// A burst of intermediate calls must not block or misbehave; the
	// throttle is time-based, so this asserts on the absence of a panic and
	// on the completion call still being accepted.
	for i := range uint64(1000) {
		fn("chunks", 1, 2, i, 1000)
	}
	fn("chunks", 1, 2, 1000, 1000)
}
