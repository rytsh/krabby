package manager

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// codeSearchTiming covers request preparation and index warmup as well as the
// actual lookup. A deferred finish also records failed/cancelled searches.
type codeSearchTiming struct {
	started, last time.Time
	stage         string
	times         map[string]time.Duration
	retried       bool
}

func newCodeSearchTiming() *codeSearchTiming {
	now := time.Now()
	return &codeSearchTiming{started: now, last: now, times: make(map[string]time.Duration)}
}

func (t *codeSearchTiming) enter(stage string) {
	now := time.Now()
	if t.stage != "" {
		t.times[t.stage] += now.Sub(t.last)
	}
	t.last, t.stage = now, stage
}

func (t *codeSearchTiming) finish(ctx context.Context, logger *slog.Logger, mode, repo, namespace string, results int, matched uint64, err error) {
	t.enter("")
	total := t.last.Sub(t.started)
	outcome := "ok"
	if errors.Is(err, context.Canceled) {
		outcome = "cancelled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		outcome = "deadline"
	} else if err != nil {
		outcome = "error"
	}
	attrs := []any{"mode", mode, "repo", repo, "namespace", namespace, "total_ms", total.Milliseconds(), "results", results, "matched", matched, "outcome", outcome, "retried", t.retried}
	for _, stage := range []string{"scope", "warm", "prepare", "index", "retry", "metadata"} {
		attrs = append(attrs, stage+"_ms", t.times[stage].Milliseconds())
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	level, message := slog.LevelDebug, "code search"
	if total >= docsSearchSlowThreshold {
		level, message = slog.LevelWarn, "slow code search"
	} else if outcome == "error" {
		level, message = slog.LevelWarn, "code search failed"
	}
	logger.Log(ctx, level, message, attrs...)
}
