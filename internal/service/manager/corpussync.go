package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
)

// docsCorpus identifies one non-repository corpus that is projected to markdown
// and pushed through the shared docs index: a web source or an API service.
//
// The two sync pipelines were written as copies of each other, and several
// stages ended up differing only in a scope prefix, a directory and the noun in
// their log lines. Keeping the varying parts in one value lets those stages be
// written once. The copies had already drifted where it mattered — a containment
// guard on os.RemoveAll existed on one side only — which is the cost this is
// meant to stop paying.
type docsCorpus struct {
	// kind is the human noun used in error text ("web source", "api service").
	kind string
	// logKey is the structured-logging attribute name for the corpus name
	// ("source", "service"), kept distinct so existing log queries still match.
	logKey string
	name   string
	scope  string
	dir    string
}

// logArgs returns the corpus identity as structured-log key/value pairs.
func (c docsCorpus) logArgs() []any { return []any{c.logKey, c.name} }

// reconcileIndex compares what the search indexes hold for a corpus against
// what its records say should be there, and reports the gaps in both
// directions: paths that must still be embedded, and paths that must be
// dropped.
//
// Paths already queued for this run are excluded from both sides, so a sweep in
// progress is not asked to redo work it is about to perform.
func (m *Manager) reconcileIndex(
	ctx context.Context,
	c docsCorpus,
	candidates map[string]bool,
	changed, removed []string,
) (missing, extra []string, err error) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

	if d.rag == nil && m.docsText == nil {
		return nil, nil, nil
	}

	indexedSets := make([]map[string]struct{}, 0, 2)

	if d.rag != nil {
		indexed, err := d.rag.IndexedPaths(ctx, c.scope)
		if err != nil {
			return nil, nil, fmt.Errorf("scan %s %s vector paths; %w", c.kind, c.name, err)
		}
		indexedSets = append(indexedSets, indexed)
	}

	if m.docsText != nil {
		indexed, err := m.docsText.IndexedPaths(ctx, c.scope)
		if err != nil {
			return nil, nil, fmt.Errorf("scan %s %s text paths; %w", c.kind, c.name, err)
		}
		indexedSets = append(indexedSets, indexed)
	}

	queuedChanges := pathSet(changed)
	queuedRemovals := pathSet(removed)
	expected := make(map[string]struct{}, len(candidates))

	for slug := range candidates {
		path := docFileName(slug)
		expected[path] = struct{}{}

		indexedEverywhere := true
		for _, indexed := range indexedSets {
			if _, ok := indexed[path]; !ok {
				indexedEverywhere = false

				break
			}
		}

		if indexedEverywhere {
			continue
		}
		if _, ok := queuedChanges[path]; ok {
			continue // already about to be embedded this run
		}
		if !fileExists(filepath.Join(c.dir, path)) {
			continue // no markdown to embed (e.g. a pending, never-fetched page)
		}

		missing = append(missing, path)
	}

	extraSet := map[string]struct{}{}
	for _, indexed := range indexedSets {
		for path := range indexed {
			if _, ok := expected[path]; ok {
				continue
			}
			if _, ok := queuedRemovals[path]; ok {
				continue
			}
			extraSet[path] = struct{}{}
		}
	}

	extra = make([]string, 0, len(extraSet))
	for path := range extraSet {
		extra = append(extra, path)
	}

	return missing, extra, nil
}

// indexPaths applies one incremental index update for a corpus, embedding the
// changed paths and dropping the removed ones.
func (m *Manager) indexPaths(
	ctx context.Context,
	c docsCorpus,
	changed, removed []string,
	updatedAt map[string]time.Time,
) error {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

	if d.rag == nil && m.docsText == nil {
		slog.Debug("docs search disabled; "+c.kind+" not indexed", c.logArgs()...)

		return nil
	}

	m.setActivity(c.scope, stepDocsIndex)
	defer m.clearActivity(c.scope, stepDocsIndex)

	// Publish live embedding progress so the UI can show a determinate bar
	// ("1200/22697 chunks embedded"). Cleared when this step returns.
	m.setProgress(c.scope, Progress{Phase: phaseIndex})
	defer m.clearProgress(c.scope, phaseIndex)

	onProgress := func(done, total int) {
		m.setProgress(c.scope, Progress{Phase: phaseIndex, Done: done, Total: total})
	}

	// Carry each document's source last-modified time onto its vectors so
	// retrieval can surface and weigh recency.
	opts := &rag.IndexOptions{
		UpdatedAt:           func(path string) time.Time { return updatedAt[path] },
		KeepMarkdownTargets: d.ragCfg.KeepMarkdownTargets,
	}

	var errs []error

	if m.docsText != nil {
		if err := m.docsText.IndexPaths(ctx, c.scope, c.dir, changed, removed, opts); err != nil {
			errs = append(errs, fmt.Errorf("index %s %s text; %w", c.kind, c.name, err))
		}
		// Query tuning is derived from the corpus, so it follows the corpus.
		// A failure here only costs lexical query speed, never correctness.
		if err := m.docsText.RefreshStats(ctx); err != nil {
			slog.Warn("refresh docs search stats", append(c.logArgs(), "error", err)...)
		}
	}

	if d.rag != nil {
		if err := d.rag.IndexPathsProgress(ctx, c.scope, c.dir, changed, removed, onProgress, opts); err != nil {
			errs = append(errs, fmt.Errorf("index %s %s vectors; %w", c.kind, c.name, err))
		}
	}

	return errors.Join(errs...)
}

// indexAll rebuilds a corpus's index from its whole directory. Failures are
// logged rather than returned: the caller is a background reindex with nowhere
// to report to, and the reconcile pass will pick the corpus up again.
func (m *Manager) indexAll(ctx context.Context, c docsCorpus) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

	if d.rag == nil && m.docsText == nil {
		slog.Debug("docs search disabled; "+c.kind+" not indexed", c.logArgs()...)

		return
	}

	m.setActivity(c.scope, stepDocsIndex)
	defer m.clearActivity(c.scope, stepDocsIndex)

	if err := m.indexDocs(ctx, d, c.scope, c.dir); err != nil {
		slog.Error("index "+c.kind, append(c.logArgs(), "error", err)...)
	}
}

// syncTask builds the queued task that syncs one corpus. forceFull is part of
// the dedup key so a full resync requested during an incremental one is queued
// rather than folded into it.
func (m *Manager) syncTask(c docsCorpus, taskKind string, forceFull bool, refresh func(context.Context, string, bool) error) queue.Task {
	key := taskKind + ":" + c.name
	spec := queue.Spec{Kind: taskKind, ID: c.scope}

	if forceFull {
		key += ":full"
		spec.Params = map[string]string{"force_full": "true"}
	}

	return queue.Task{
		ID:    c.scope,
		Kind:  taskKind,
		Title: "Sync " + c.scope,
		Key:   key,
		Spec:  spec,
		Run: func(ctx context.Context) error {
			if err := refresh(ctx, c.name, forceFull); err != nil {
				slog.Error("refresh "+c.kind, append(c.logArgs(), "error", err)...)

				return err
			}

			return nil
		},
	}
}
