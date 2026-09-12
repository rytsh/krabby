package manager

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// searchTimeout bounds one index-only search (normal text, regex).
//
// Every stage of a search costs time proportional to the corpus, and the HTTP
// server sets no write deadline, so without this a pathological query returns
// never rather than slowly: the request goroutine stays alive until the client
// gives up, and the deferred finish() that would have named the slow stage
// never runs. A deadline converts a silent hang into a "deadline" outcome with
// the per-stage breakdown attached, which is the difference between a report
// saying "it froze" and one saying which stage ate the time.
const searchTimeout = 30 * time.Second

// semanticSearchTimeout bounds searches that call out to an embedder or an
// LLM. Those have their own per-call timeouts (30s for the embedder), so the
// budget here only has to stop a request that is stuck between them.
const semanticSearchTimeout = 2 * time.Minute

// withSearchTimeout applies d, unless the caller already imposed a deadline at
// least as strict — a caller that wants a shorter search is never overridden.
func withSearchTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= d {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, d)
}

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
