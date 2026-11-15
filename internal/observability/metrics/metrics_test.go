package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rytsh/krabby/internal/observability/metrics"
)

func newRecorder(t *testing.T) (*metrics.Recorder, func() map[string]metricdata.Metrics) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	rec, err := metrics.New(provider)
	if err != nil {
		t.Fatal(err)
	}

	collect := func() map[string]metricdata.Metrics {
		t.Helper()

		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}

		out := map[string]metricdata.Metrics{}
		for _, scope := range rm.ScopeMetrics {
			for _, m := range scope.Metrics {
				out[m.Name] = m
			}
		}

		return out
	}

	return rec, collect
}

func attrsKey(set attribute.Set) string {
	return set.Encoded(attribute.DefaultEncoder())
}

// histogram returns count and sum per attribute set.
func histogram(t *testing.T, m metricdata.Metrics) map[string][2]float64 {
	t.Helper()

	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
	}

	out := map[string][2]float64{}
	for _, p := range h.DataPoints {
		out[attrsKey(p.Attributes)] = [2]float64{float64(p.Count), p.Sum}
	}

	return out
}

func points(t *testing.T, m metricdata.Metrics) map[string]int64 {
	t.Helper()

	out := map[string]int64{}

	switch data := m.Data.(type) {
	case metricdata.Gauge[int64]:
		for _, p := range data.DataPoints {
			out[attrsKey(p.Attributes)] = p.Value
		}
	case metricdata.Sum[int64]:
		if !data.IsMonotonic {
			t.Errorf("%s is not monotonic", m.Name)
		}
		for _, p := range data.DataPoints {
			out[attrsKey(p.Attributes)] = p.Value
		}
	default:
		t.Fatalf("%s is %T, want an int64 gauge or sum", m.Name, m.Data)
	}

	return out
}

func TestTaskFinishedRecordsWaitAndRun(t *testing.T) {
	rec, collect := newRecorder(t)

	enqueued := time.Unix(1000, 0)
	rec.TaskFinished(context.Background(), "refresh", "done", enqueued, enqueued.Add(3*time.Second), enqueued.Add(10*time.Second))
	rec.TaskFinished(context.Background(), "refresh", "error", enqueued, enqueued.Add(time.Second), enqueued.Add(2*time.Second))

	got := collect()

	wait := histogram(t, got["krabby.queue.task.wait.duration"])
	if w := wait["kind=refresh"]; w != [2]float64{2, 4} {
		t.Errorf("wait = %v, want 2 observations summing 4s", w)
	}

	run := histogram(t, got["krabby.queue.task.run.duration"])
	if r := run["kind=refresh,state=done"]; r != [2]float64{1, 7} {
		t.Errorf("run done = %v, want 1 observation of 7s", r)
	}
	if r := run["kind=refresh,state=error"]; r != [2]float64{1, 1} {
		t.Errorf("run error = %v, want 1 observation of 1s", r)
	}
	if u := got["krabby.queue.task.run.duration"].Unit; u != "s" {
		t.Errorf("unit = %q, want s", u)
	}
}

// A task cancelled before it started neither finished waiting nor ran;
// recording it would report a wait that was really an abandonment.
func TestTaskCancelledWhileQueuedIsNotRecorded(t *testing.T) {
	rec, collect := newRecorder(t)

	rec.TaskFinished(context.Background(), "refresh", "canceled", time.Unix(1000, 0), time.Time{}, time.Unix(1005, 0))

	got := collect()
	for _, name := range []string{"krabby.queue.task.wait.duration", "krabby.queue.task.run.duration"} {
		if m, ok := got[name]; ok && len(histogram(t, m)) > 0 {
			t.Errorf("%s recorded a task that never started", name)
		}
	}
}

func TestStageAndSearchDurations(t *testing.T) {
	rec, collect := newRecorder(t)

	rec.StageFinished(context.Background(), "graph", metrics.OutcomeOK, 4*time.Second)
	rec.StageFinished(context.Background(), "docs", metrics.OutcomeError, 90*time.Second)
	rec.SearchFinished(context.Background(), "code", "regex", metrics.OutcomeOK, 250*time.Millisecond)
	rec.SearchFinished(context.Background(), "docs", "hybrid", metrics.OutcomeDeadline, 2*time.Minute)

	got := collect()

	stage := histogram(t, got["krabby.stage.duration"])
	if s := stage["outcome=ok,stage=graph"]; s != [2]float64{1, 4} {
		t.Errorf("graph stage = %v", s)
	}
	if s := stage["outcome=error,stage=docs"]; s != [2]float64{1, 90} {
		t.Errorf("docs stage = %v", s)
	}

	search := histogram(t, got["krabby.search.duration"])
	if s := search["kind=code,mode=regex,outcome=ok"]; s != [2]float64{1, 0.25} {
		t.Errorf("regex search = %v", s)
	}
	if s := search["kind=docs,mode=hybrid,outcome=deadline"]; s != [2]float64{1, 120} {
		t.Errorf("hybrid search = %v", s)
	}
}

// The default boundaries assume milliseconds; in seconds they would drop a
// 4s and a 20m stage into the same bucket.
func TestDurationBucketsResolveSeconds(t *testing.T) {
	rec, collect := newRecorder(t)

	rec.StageFinished(context.Background(), "graph", metrics.OutcomeOK, 4*time.Second)
	rec.StageFinished(context.Background(), "graph", metrics.OutcomeOK, 20*time.Minute)
	rec.SearchFinished(context.Background(), "code", "text", metrics.OutcomeOK, 8*time.Millisecond)
	rec.SearchFinished(context.Background(), "code", "text", metrics.OutcomeOK, 3*time.Second)

	got := collect()
	for _, name := range []string{"krabby.stage.duration", "krabby.search.duration"} {
		h := got[name].Data.(metricdata.Histogram[float64])

		occupied := 0
		for _, c := range h.DataPoints[0].BucketCounts {
			if c > 0 {
				occupied++
			}
		}
		if occupied != 2 {
			t.Errorf("%s put two very different durations into %d bucket(s)", name, occupied)
		}
	}
}

func TestOutcome(t *testing.T) {
	for want, err := range map[string]error{
		metrics.OutcomeOK:        nil,
		metrics.OutcomeCancelled: fmt.Errorf("search; %w", context.Canceled),
		metrics.OutcomeDeadline:  fmt.Errorf("search; %w", context.DeadlineExceeded),
		metrics.OutcomeError:     errors.New("index unavailable"),
	} {
		if got := metrics.Outcome(err); got != want {
			t.Errorf("Outcome(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestObserveQueueReadsLiveDepth(t *testing.T) {
	rec, collect := newRecorder(t)

	depth := metrics.QueueDepth{Limit: 3, Running: 2, Pending: 7}
	if err := rec.ObserveQueue(func() metrics.QueueDepth { return depth }); err != nil {
		t.Fatal(err)
	}

	got := collect()
	tasks := points(t, got["krabby.queue.tasks"])
	if tasks["state=queued"] != 7 || tasks["state=running"] != 2 {
		t.Errorf("tasks = %v, want 7 queued and 2 running", tasks)
	}
	if limit := points(t, got["krabby.queue.limit"])[""]; limit != 3 {
		t.Errorf("limit = %d, want 3", limit)
	}

	// Read at collection time, not captured at registration.
	depth = metrics.QueueDepth{Limit: 5, Running: 0, Pending: 0}
	tasks = points(t, collect()["krabby.queue.tasks"])
	if tasks["state=queued"] != 0 || tasks["state=running"] != 0 {
		t.Errorf("tasks after drain = %v, want zeros", tasks)
	}
}

func TestObserveGraphCache(t *testing.T) {
	rec, collect := newRecorder(t)

	if err := rec.ObserveGraphCache(func() metrics.GraphCache {
		return metrics.GraphCache{Hits: 40, Misses: 3, Evictions: 1, Entries: 2, Bytes: 1 << 20, MaxBytes: 8 << 20}
	}); err != nil {
		t.Fatal(err)
	}

	got := collect()
	lookups := points(t, got["krabby.graph.cache.lookups"])
	if lookups["result=hit"] != 40 || lookups["result=miss"] != 3 {
		t.Errorf("lookups = %v", lookups)
	}
	for name, want := range map[string]int64{
		"krabby.graph.cache.evictions": 1,
		"krabby.graph.cache.entries":   2,
		"krabby.graph.cache.size":      1 << 20,
		"krabby.graph.cache.limit":     8 << 20,
	} {
		if v := points(t, got[name])[""]; v != want {
			t.Errorf("%s = %d, want %d", name, v, want)
		}
	}
}

// An unbounded cache is configured with a non-positive budget; a negative
// byte gauge would be nonsense on a dashboard.
func TestGraphCacheLimitIsNeverNegative(t *testing.T) {
	rec, collect := newRecorder(t)

	if err := rec.ObserveGraphCache(func() metrics.GraphCache { return metrics.GraphCache{MaxBytes: -1} }); err != nil {
		t.Fatal(err)
	}

	if v := points(t, collect()["krabby.graph.cache.limit"])[""]; v != 0 {
		t.Errorf("limit = %d, want 0 for an unbounded cache", v)
	}
}

// Callers hold a possibly-nil recorder and must not have to check it.
func TestNilRecorderRecordsNothing(t *testing.T) {
	var rec *metrics.Recorder

	rec.TaskFinished(context.Background(), "refresh", "done", time.Now(), time.Now(), time.Now())
	rec.StageFinished(context.Background(), "graph", metrics.OutcomeOK, time.Second)
	rec.SearchFinished(context.Background(), "code", "text", metrics.OutcomeOK, time.Second)

	if err := rec.ObserveQueue(func() metrics.QueueDepth { return metrics.QueueDepth{} }); err != nil {
		t.Errorf("ObserveQueue on nil recorder: %v", err)
	}
	if err := rec.ObserveGraphCache(func() metrics.GraphCache { return metrics.GraphCache{} }); err != nil {
		t.Errorf("ObserveGraphCache on nil recorder: %v", err)
	}
}
