package coderag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/rag"
)

// The code index needs the same corpus-derived query tuning the documentation
// index has, and needs it more: a source chunk's vocabulary is dominated by
// language keywords, so a question rewritten into an OR chain routinely
// contains `func`, `return` or `error` — terms present in most of the corpus,
// each costing a posting-list walk proportional to the whole index and each
// contributing almost nothing to the ranking.
//
// The threshold, sample size and cap are shared with the docs index through
// rag.FrequentTermSampler; only the bucket and the fields a chunk contributes
// are local.

const (
	codeStatsBucketName = "code_search_stats"
	codeStatsRecordID   = "corpus"
)

// codeStats is the persisted frequent-term set for the code index.
type codeStats struct {
	ID string `bw:"id,pk"`
	// Total is the corpus size the set was computed from, used to detect that
	// it has drifted far enough to be worth recomputing.
	Total int `bw:"total"`
	// Sampled is how many chunks were inspected to produce Frequent.
	Sampled int `bw:"sampled"`
	// Frequent lists the lower-cased terms found in most sampled chunks.
	Frequent  []string  `bw:"frequent"`
	UpdatedAt time.Time `bw:"updated_at"`
}

// statsCache memoises the persisted set. Search reads it on every query while
// it only changes when the index is rebuilt.
type statsCache struct {
	mu     sync.RWMutex
	terms  rag.StopWords
	loaded bool
}

func (c *statsCache) get() (rag.StopWords, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.terms, c.loaded
}

func (c *statsCache) set(terms rag.StopWords) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.terms = terms
	c.loaded = true
}

func (c *statsCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.terms = nil
	c.loaded = false
}

// FrequentTerms returns the terms the indexed source itself shows to be too
// common to be worth searching. Nil means the statistics have not been
// computed yet, which simply means no filtering.
func (s *TextStore) FrequentTerms(ctx context.Context) rag.StopWords {
	if terms, ok := s.stats.get(); ok {
		return terms
	}

	rec, err := s.statsBucket.Get(ctx, codeStatsRecordID)
	if err != nil && !errors.Is(err, bw.ErrNotFound) {
		// Statistics are an optimisation; a read failure must not fail search.
		return nil
	}

	var terms rag.StopWords
	if rec != nil {
		terms = rag.NewStopWords(rec.Frequent)
	}

	s.stats.set(terms)

	return terms
}

// RefreshStats recomputes the frequent-term set when the corpus has changed
// enough to matter. It is local-only (no LLM or embedder calls), cheap when
// nothing changed, and safe to run in the background after an index rebuild.
func (s *TextStore) RefreshStats(ctx context.Context) error {
	count, err := s.bucket.Count(ctx, nil)
	if err != nil {
		return fmt.Errorf("count code search chunks; %w", err)
	}

	total := int(count)
	if total == 0 {
		if err := s.statsBucket.Delete(ctx, codeStatsRecordID); err != nil && !errors.Is(err, bw.ErrNotFound) {
			return fmt.Errorf("clear code search stats; %w", err)
		}
		s.stats.invalidate()

		return nil
	}

	if prev, err := s.statsBucket.Get(ctx, codeStatsRecordID); err == nil && prev != nil && rag.StatsFresh(prev.Total, total) {
		return nil
	}

	// The walk is strided across the whole bucket. Chunk ids are
	// repo-prefixed, so a prefix of the walk would measure one repository's
	// vocabulary rather than the corpus's.
	sampler := rag.NewFrequentTermSampler(total)

	// Only the chunk text is offered. Path and symbol are indexed too, but a
	// path segment shared by most of a repository ("internal", "service") is
	// exactly the term a path-scoped search needs, and dropping it would make
	// that search impossible rather than merely slower.
	if err := s.bucket.Walk(ctx, nil, func(record *textRecord) error {
		sampler.Observe(record.Snippet)

		return nil
	}); err != nil {
		return fmt.Errorf("walk code search chunks; %w", err)
	}

	frequent, sampled := sampler.Result()
	if sampled == 0 {
		return nil
	}

	rec := &codeStats{
		ID:        codeStatsRecordID,
		Total:     total,
		Sampled:   sampled,
		Frequent:  frequent,
		UpdatedAt: time.Now(),
	}
	if err := s.statsBucket.Insert(ctx, rec); err != nil {
		return fmt.Errorf("save code search stats; %w", err)
	}

	s.stats.set(rag.NewStopWords(frequent))

	slog.Info("code search stats refreshed",
		"chunks", total, "sampled", sampled, "frequent_terms", len(frequent))

	return nil
}

// registerStatsBucket registers the statistics bucket. It lives beside the
// record type it owns so the two cannot drift.
func registerStatsBucket(db *bw.DB) (*bw.Bucket[codeStats], error) {
	bucket, err := bw.RegisterBucket[codeStats](db, codeStatsBucketName)
	if err != nil {
		return nil, fmt.Errorf("register code search stats bucket; %w", err)
	}

	return bucket, nil
}
