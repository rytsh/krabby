// Package metrics defines krabby's own OpenTelemetry instruments: how deep the
// work queue is, how long tasks, pipeline stages and searches take, and how
// well the parsed-graph cache is doing.
//
// Nothing here costs anything until telemetry.collector is configured. Without
// it the provider is a no-op and every call below returns immediately, which
// keeps the model-free core path as cheap as it was.
//
// Attribute values are always drawn from fixed sets in the code (a task kind, a
// stage name, a search mode), never from a request. A repository id or a query
// as a label would mint one time series per value and take the metrics backend
// down with it.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/rytsh/krabby"

// Outcomes shared by every duration instrument.
const (
	OutcomeOK        = "ok"
	OutcomeError     = "error"
	OutcomeCancelled = "cancelled"
	OutcomeDeadline  = "deadline"
	// OutcomeInvalid is a request the caller got wrong: an unknown repository,
	// a pattern that does not compile, an empty question. Counting those as
	// errors would make a typo look like the service failing.
	OutcomeInvalid = "invalid"
)

// errInvalid marks an error as the caller's mistake; see Invalid.
var errInvalid = errors.New("invalid request")

type invalidError struct{ error }

func (e invalidError) Unwrap() []error { return []error{e.error, errInvalid} }

// Invalid marks err as caller error, so Outcome reports it as
// OutcomeInvalid. The message is unchanged, and errors.Is/As still see err.
func Invalid(err error) error {
	if err == nil {
		return nil
	}

	return invalidError{err}
}

// Bucket boundaries in seconds. The SDK default is tuned for milliseconds and
// would put every observation here in its first two buckets.
var (
	// A search is interactive: the interesting range is a few milliseconds up
	// to the request timeout.
	searchBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	// Background work runs from under a second (an up-to-date refresh) to the
	// better part of an hour (documentation for a large repository).
	workBuckets = []float64{0.1, 0.5, 1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600}
)

// Recorder holds the instruments. A nil *Recorder is valid and records
// nothing, so callers never have to check whether telemetry was wired.
type Recorder struct {
	meter metric.Meter

	taskWait metric.Float64Histogram
	taskRun  metric.Float64Histogram
	stage    metric.Float64Histogram
	search   metric.Float64Histogram
}

// New creates the instruments on provider.
func New(provider metric.MeterProvider) (*Recorder, error) {
	r := &Recorder{meter: provider.Meter(scopeName)}

	var err error

	if r.taskWait, err = r.meter.Float64Histogram("krabby.queue.task.wait.duration",
		metric.WithDescription("Time a background task spent queued before it started."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(workBuckets...),
	); err != nil {
		return nil, fmt.Errorf("create task wait histogram; %w", err)
	}

	if r.taskRun, err = r.meter.Float64Histogram("krabby.queue.task.run.duration",
		metric.WithDescription("Time a background task spent running."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(workBuckets...),
	); err != nil {
		return nil, fmt.Errorf("create task run histogram; %w", err)
	}

	if r.stage, err = r.meter.Float64Histogram("krabby.stage.duration",
		metric.WithDescription("Time one repository pipeline stage took."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(workBuckets...),
	); err != nil {
		return nil, fmt.Errorf("create stage histogram; %w", err)
	}

	if r.search, err = r.meter.Float64Histogram("krabby.search.duration",
		metric.WithDescription("Time one code or documentation search took."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(searchBuckets...),
	); err != nil {
		return nil, fmt.Errorf("create search histogram; %w", err)
	}

	return r, nil
}

// Outcome classifies an error for the outcome attribute. Cancellation and a
// missed deadline are kept apart from a failure: the first is somebody
// changing their mind, the second is the system being too slow.
func Outcome(err error) string {
	switch {
	case err == nil:
		return OutcomeOK
	case errors.Is(err, context.Canceled):
		return OutcomeCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeDeadline
	case errors.Is(err, errInvalid):
		return OutcomeInvalid
	default:
		return OutcomeError
	}
}

// TaskFinished records a task that ran. started is zero for a task cancelled
// while it was still queued; such a task never waited to completion nor ran,
// so it contributes to neither histogram.
func (r *Recorder) TaskFinished(ctx context.Context, kind, state string, enqueued, started, ended time.Time) {
	if r == nil || started.IsZero() {
		return
	}

	r.taskWait.Record(ctx, started.Sub(enqueued).Seconds(),
		metric.WithAttributes(attribute.String("kind", kind)))

	if !ended.IsZero() {
		r.taskRun.Record(ctx, ended.Sub(started).Seconds(), metric.WithAttributes(
			attribute.String("kind", kind),
			attribute.String("state", state),
		))
	}
}

// StageFinished records one pipeline stage of one repository.
func (r *Recorder) StageFinished(ctx context.Context, stage, outcome string, took time.Duration) {
	if r == nil {
		return
	}

	r.stage.Record(ctx, took.Seconds(), metric.WithAttributes(
		attribute.String("stage", stage),
		attribute.String("outcome", outcome),
	))
}

// SearchFinished records one search. kind is what was searched ("code",
// "docs") and mode how ("text", "regex", "semantic", "lexical", "hybrid").
func (r *Recorder) SearchFinished(ctx context.Context, kind, mode, outcome string, took time.Duration) {
	if r == nil {
		return
	}

	r.search.Record(ctx, took.Seconds(), metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("mode", mode),
		attribute.String("outcome", outcome),
	))
}

// QueueDepth is the work queue at one instant.
type QueueDepth struct {
	Limit, Running, Pending int
}

// ObserveQueue reports the queue's depth each time metrics are collected. read
// is called from the collection goroutine and must be cheap and safe to call
// concurrently with the queue.
func (r *Recorder) ObserveQueue(read func() QueueDepth) error {
	if r == nil {
		return nil
	}

	tasks, err := r.meter.Int64ObservableGauge("krabby.queue.tasks",
		metric.WithDescription("Background tasks currently queued or running."),
		metric.WithUnit("{task}"),
	)
	if err != nil {
		return fmt.Errorf("create queue task gauge; %w", err)
	}

	limit, err := r.meter.Int64ObservableGauge("krabby.queue.limit",
		metric.WithDescription("How many background tasks may run at once."),
		metric.WithUnit("{task}"),
	)
	if err != nil {
		return fmt.Errorf("create queue limit gauge; %w", err)
	}

	_, err = r.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		d := read()
		o.ObserveInt64(tasks, int64(d.Pending), metric.WithAttributes(attribute.String("state", "queued")))
		o.ObserveInt64(tasks, int64(d.Running), metric.WithAttributes(attribute.String("state", "running")))
		o.ObserveInt64(limit, int64(d.Limit))

		return nil
	}, tasks, limit)
	if err != nil {
		return fmt.Errorf("register queue callback; %w", err)
	}

	return nil
}

// GraphCache is the parsed-graph cache at one instant. The counters are
// cumulative since start.
type GraphCache struct {
	Hits, Misses, Evictions uint64
	Entries                 int
	Bytes, MaxBytes         int64
}

// ObserveGraphCache reports the graph cache each time metrics are collected.
// A falling hit ratio with a steady eviction rate means the cache budget is
// too small for the repositories being queried.
func (r *Recorder) ObserveGraphCache(read func() GraphCache) error {
	if r == nil {
		return nil
	}

	lookups, err := r.meter.Int64ObservableCounter("krabby.graph.cache.lookups",
		metric.WithDescription("Graph cache lookups, by whether the parsed graph was already in memory."),
		metric.WithUnit("{lookup}"),
	)
	if err != nil {
		return fmt.Errorf("create graph cache lookup counter; %w", err)
	}

	evictions, err := r.meter.Int64ObservableCounter("krabby.graph.cache.evictions",
		metric.WithDescription("Parsed graphs dropped from memory to stay within the cache budget."),
		metric.WithUnit("{graph}"),
	)
	if err != nil {
		return fmt.Errorf("create graph cache eviction counter; %w", err)
	}

	entries, err := r.meter.Int64ObservableGauge("krabby.graph.cache.entries",
		metric.WithDescription("Parsed graphs currently held in memory."),
		metric.WithUnit("{graph}"),
	)
	if err != nil {
		return fmt.Errorf("create graph cache entry gauge; %w", err)
	}

	size, err := r.meter.Int64ObservableGauge("krabby.graph.cache.size",
		metric.WithDescription("Estimated memory held by cached graphs."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("create graph cache size gauge; %w", err)
	}

	budget, err := r.meter.Int64ObservableGauge("krabby.graph.cache.limit",
		metric.WithDescription("Memory budget of the graph cache; 0 means unbounded."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("create graph cache limit gauge; %w", err)
	}

	_, err = r.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c := read()
		//nolint:gosec // cumulative counters; overflowing int64 is not reachable in a process lifetime
		o.ObserveInt64(lookups, int64(c.Hits), metric.WithAttributes(attribute.String("result", "hit")))
		//nolint:gosec // as above
		o.ObserveInt64(lookups, int64(c.Misses), metric.WithAttributes(attribute.String("result", "miss")))
		//nolint:gosec // as above
		o.ObserveInt64(evictions, int64(c.Evictions))
		o.ObserveInt64(entries, int64(c.Entries))
		o.ObserveInt64(size, c.Bytes)
		o.ObserveInt64(budget, max(c.MaxBytes, 0))

		return nil
	}, lookups, evictions, entries, size, budget)
	if err != nil {
		return fmt.Errorf("register graph cache callback; %w", err)
	}

	return nil
}
