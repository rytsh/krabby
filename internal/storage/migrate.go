package storage

import (
	"log/slog"
	"sync"
	"time"

	"github.com/rakunlabs/bw"
)

// migrationLogInterval is how often a running migration reports progress.
//
// bw invokes the progress hook once per transaction, and a bucket carrying a
// vector index commits one record at a time — so a corpus-sized migration
// would emit a line per record. Throttling to a wall-clock interval keeps the
// signal (it is still running, and here is how far it has got) without the
// noise.
const migrationLogInterval = 10 * time.Second

// MigrationProgress returns a bw progress hook that logs how far a schema
// migration has got.
//
// A migration blocks startup, so without it the process looks hung: krabby
// prints nothing between "starting" and however many minutes later it
// finishes, and an operator has no way to tell a slow migration from a stuck
// one. The percentage and the rate are what answer "should I wait or
// intervene".
func MigrationProgress() bw.MigrationProgress {
	var (
		mu       sync.Mutex
		started  time.Time
		lastLog  time.Time
		lastStep [2]uint64
	)

	return func(bucket string, fromV, toV, processed, total uint64) {
		mu.Lock()
		defer mu.Unlock()

		now := time.Now()

		// Each (fromV, toV) pair is a separate pass over the bucket, so
		// timing restarts with it.
		if step := [2]uint64{fromV, toV}; step != lastStep {
			lastStep, started, lastLog = step, now, time.Time{}
		}

		done := processed >= total
		if !done && !lastLog.IsZero() && now.Sub(lastLog) < migrationLogInterval {
			return
		}
		lastLog = now

		elapsed := now.Sub(started)

		attrs := []any{
			"bucket", bucket,
			"from_version", fromV,
			"to_version", toV,
			"processed", processed,
			"total", total,
			"elapsed", elapsed.Round(time.Second).String(),
		}

		if total > 0 {
			attrs = append(attrs, "percent", processed*100/total)
		}

		// A remaining-time estimate is only meaningful once there is a rate
		// to extrapolate from.
		if processed > 0 && processed < total && elapsed > 0 {
			perRecord := elapsed / time.Duration(processed)
			attrs = append(attrs, "eta", (perRecord * time.Duration(total-processed)).Round(time.Second).String())
		}

		if done {
			slog.Info("database migration finished", attrs...)

			return
		}

		slog.Info("database migration in progress", attrs...)
	}
}
