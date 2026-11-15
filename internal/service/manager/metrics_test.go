package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rytsh/krabby/internal/observability/metrics"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// metricsHarness collects what a manager records through a real SDK provider.
type metricsHarness struct {
	t      *testing.T
	rec    *metrics.Recorder
	reader *sdkmetric.ManualReader
}

func newMetricsHarness(t *testing.T) *metricsHarness {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	rec, err := metrics.New(provider)
	if err != nil {
		t.Fatal(err)
	}

	return &metricsHarness{t: t, rec: rec, reader: reader}
}

func (h *metricsHarness) collect(name string) metricdata.Metrics {
	h.t.Helper()

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatal(err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m
			}
		}
	}

	return metricdata.Metrics{Name: name}
}

// counts returns how many observations each attribute set received.
func (h *metricsHarness) counts(name string) map[string]uint64 {
	h.t.Helper()

	out := map[string]uint64{}

	data, ok := h.collect(name).Data.(metricdata.Histogram[float64])
	if !ok {
		return out
	}
	for _, p := range data.DataPoints {
		out[p.Attributes.Encoded(attribute.DefaultEncoder())] = p.Count
	}

	return out
}

func (h *metricsHarness) values(name string) map[string]int64 {
	h.t.Helper()

	out := map[string]int64{}

	switch data := h.collect(name).Data.(type) {
	case metricdata.Gauge[int64]:
		for _, p := range data.DataPoints {
			out[p.Attributes.Encoded(attribute.DefaultEncoder())] = p.Value
		}
	case metricdata.Sum[int64]:
		for _, p := range data.DataPoints {
			out[p.Attributes.Encoded(attribute.DefaultEncoder())] = p.Value
		}
	}

	return out
}

// Stages report under their own name and with how they ended, including a
// stage stopped by its job being cancelled.
func TestStagesAreMeasured(t *testing.T) {
	h := newMetricsHarness(t)

	m, _ := newStageTestManager(t)
	m.activity = map[string]map[string]struct{}{}
	m.metrics = h.rec
	repo := &registry.Repo{ID: "owner/repo"}

	if err := m.runStage(context.Background(), repo, registry.StageDocsIndex, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := m.runStage(context.Background(), repo, registry.StageDocs, func() error { return errors.New("llm unavailable") }); err == nil {
		t.Fatal("failing stage returned no error")
	}

	// A cancelled job usually surfaces as whatever the interrupted work
	// returned, not as a context error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.runStage(ctx, repo, registry.StageCodeIndex, func() error { return errors.New("signal: killed") }); err == nil {
		t.Fatal("cancelled stage returned no error")
	}

	got := h.counts("krabby.stage.duration")
	for _, want := range []string{
		"outcome=ok,stage=docs_index",
		"outcome=error,stage=docs",
		"outcome=cancelled,stage=code_index",
	} {
		if got[want] != 1 {
			t.Errorf("stage series %q has %d observations, want 1 (all: %v)", want, got[want], got)
		}
	}
}

func TestCodeSearchIsMeasured(t *testing.T) {
	h := newMetricsHarness(t)

	m, ctx := codeSearchFixture(t, "acme/api", []vectorstore.Item{
		codeChunk("acme/api", "auth.go", "Middleware", 1, "func Middleware(next http.Handler) http.Handler {\n\treturn next\n}\n"),
	})
	m.metrics = h.rec

	if _, err := m.SearchCodeText(ctx, "acme/api", "", "Middleware", coderag.TextSearchOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SearchCodeRegex(ctx, "acme/api", "", `func \w+\(`, coderag.RegexOptions{}); err != nil {
		t.Fatal(err)
	}
	// A repository that is not tracked is refused: still a search that was
	// attempted and failed, and it must not vanish from the numbers.
	if _, err := m.SearchCodeText(ctx, "acme/missing", "", "Middleware", coderag.TextSearchOptions{}); err == nil {
		t.Fatal("search in an untracked repository succeeded")
	}

	got := h.counts("krabby.search.duration")
	for want, n := range map[string]uint64{
		"kind=code,mode=text,outcome=ok":    1,
		"kind=code,mode=regex,outcome=ok":   1,
		"kind=code,mode=text,outcome=error": 1,
	} {
		if got[want] != n {
			t.Errorf("search series %q has %d observations, want %d (all: %v)", want, got[want], n, got)
		}
	}
}

// A request naming a mode that does not exist is rejected before it becomes a
// search. Recording it would let any caller mint a new time series per typo.
func TestUnknownDocsSearchModeIsNotALabel(t *testing.T) {
	h := newMetricsHarness(t)

	m, ctx := codeSearchFixture(t, "acme/api", nil)
	m.metrics = h.rec

	if _, err := m.SearchDocs(ctx, "", "", "", "telepathy", "how does auth work", 0); err == nil {
		t.Fatal("unknown docs search mode was accepted")
	}

	if got := h.counts("krabby.search.duration"); len(got) != 0 {
		t.Errorf("rejected mode produced metric series: %v", got)
	}
}

// SetMetrics is what startup calls: after it, the queue reports tasks and its
// depth, and the graph cache reports itself, without any further wiring.
func TestSetMetricsWiresQueueAndGraphCache(t *testing.T) {
	h := newMetricsHarness(t)

	m := &Manager{engine: graphquery.NewEngine(1 << 20), queue: queue.New(context.Background(), 2)}
	t.Cleanup(m.queue.Close)

	if err := m.SetMetrics(h.rec); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	started := make(chan struct{})
	running := m.queue.Submit(queue.Task{ID: "a", Kind: taskKindRefresh, Run: func(context.Context) error {
		close(started)
		<-release

		return nil
	}})
	<-started

	if got := h.values("krabby.queue.tasks"); got["state=running"] != 1 || got["state=queued"] != 0 {
		t.Errorf("queue tasks while one runs = %v", got)
	}
	if got := h.values("krabby.queue.limit"); got[""] != 2 {
		t.Errorf("queue limit = %v, want 2", got)
	}

	close(release)
	<-running.Done()

	deadline := time.Now().Add(time.Second)
	for h.counts("krabby.queue.task.run.duration")["kind=refresh,state=done"] != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("finished task was not recorded: %v", h.counts("krabby.queue.task.run.duration"))
		}
		time.Sleep(time.Millisecond)
	}
	if got := h.counts("krabby.queue.task.wait.duration"); got["kind=refresh"] != 1 {
		t.Errorf("task wait = %v, want one refresh observation", got)
	}

	if got := h.values("krabby.graph.cache.limit"); got[""] != 1<<20 {
		t.Errorf("graph cache limit = %v, want the engine's budget", got)
	}
	if got := h.values("krabby.graph.cache.entries"); got[""] != 0 {
		t.Errorf("graph cache entries = %v, want 0 on an empty engine", got)
	}
}
