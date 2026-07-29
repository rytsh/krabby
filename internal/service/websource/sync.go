package websource

import (
	"fmt"
	"strings"
	"time"

	"github.com/worldline-go/hardloop"
)

// DefaultFullResyncSchedule runs a full reconciliation daily at 02:00 in the
// server's local timezone. Incremental syncs between those runs remain cheap.
const DefaultFullResyncSchedule = "0 2 * * *"

// FullResyncSchedule returns the effective cron schedule. legacyEvery supports
// persisted provider configs from before full resyncs became cron-based.
func FullResyncSchedule(schedule, legacyEvery string) string {
	if schedule = strings.TrimSpace(schedule); schedule != "" {
		return schedule
	}
	if d, err := time.ParseDuration(strings.TrimSpace(legacyEvery)); err == nil && d > 0 {
		return "@every " + d.String()
	}

	return DefaultFullResyncSchedule
}

// ValidateFullResyncSchedule checks one hardloop cron expression.
func ValidateFullResyncSchedule(schedule string) error {
	if _, err := hardloop.ParseStandard(schedule); err != nil {
		return fmt.Errorf("invalid full resync schedule %q: %w", schedule, err)
	}

	return nil
}

// FullResyncDue reports whether the first cron activation after lastFull has
// arrived. A zero lastFull always forces the initial complete pass.
func FullResyncDue(lastFull time.Time, schedule string, now time.Time) bool {
	if lastFull.IsZero() {
		return true
	}
	parsed, err := hardloop.ParseStandard(schedule)
	if err != nil {
		parsed, _ = hardloop.ParseStandard(DefaultFullResyncSchedule)
	}
	next := parsed.Next(lastFull)

	return !next.IsZero() && !next.After(now)
}
