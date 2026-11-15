package manager

import (
	"context"
	"fmt"

	"github.com/rytsh/krabby/internal/observability/metrics"
	"github.com/rytsh/krabby/internal/service/queue"
)

// Search kinds for the search-duration metric.
const (
	searchKindCode = "code"
	searchKindDocs = "docs"
)

// SetMetrics wires the recorder into the queue and the graph cache. Call it
// once at startup; without it nothing is measured and nothing breaks.
func (m *Manager) SetMetrics(rec *metrics.Recorder) error {
	m.metrics = rec

	m.queue.SetObserver(func(it queue.Item) {
		// The task's own context is gone by the time it is reported, and a
		// measurement must not be dropped because the work it describes was
		// cancelled.
		rec.TaskFinished(context.Background(), it.Kind, string(it.State), it.EnqueuedAt, it.StartedAt, it.EndedAt)
	})

	if err := rec.ObserveQueue(func() metrics.QueueDepth {
		limit, running, pending := m.queue.Depth()

		return metrics.QueueDepth{Limit: limit, Running: running, Pending: pending}
	}); err != nil {
		return fmt.Errorf("observe task queue; %w", err)
	}

	if err := rec.ObserveGraphCache(func() metrics.GraphCache {
		s := m.engine.Stats()

		return metrics.GraphCache{
			Hits: s.Hits, Misses: s.Misses, Evictions: s.Evictions,
			Entries: s.Entries, Bytes: s.Bytes, MaxBytes: s.MaxBytes,
		}
	}); err != nil {
		return fmt.Errorf("observe graph cache; %w", err)
	}

	return nil
}
