package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/searchutil"
)

const (
	docsStatsBucketName = "docs_search_stats"
	docsStatsRecordID   = "corpus"
)

// textStats is the corpus-derived query tuning for the lexical index.
type textStats struct {
	ID string `bw:"id,pk"`
	// Total is the corpus size the set was computed from, used to detect that
	// it has drifted far enough to be worth recomputing.
	Total int `bw:"total"`
	// Sampled is how many documents were inspected to produce Frequent.
	Sampled int `bw:"sampled"`
	// Frequent lists the lower-cased terms above the shared sampler's
	// document-frequency threshold.
	Frequent  []string  `bw:"frequent"`
	UpdatedAt time.Time `bw:"updated_at"`
}

// FrequentTerms returns the corpus-derived terms that are too common to be
// worth searching, for use as searchutil.LexicalQuery's stop set. It returns nil
// when statistics have not been computed yet, which simply means no filtering.
//
// This is deliberately derived from the corpus rather than from a built-in word
// list: it needs no knowledge of the corpus language, and it adapts to the
// domain, where a word like "payment" in a payments-only collection is exactly
// as uninformative as "the".
func (s *TextStore) FrequentTerms(ctx context.Context) searchutil.StopWords {
	terms, ok, gen := s.stats.Get()
	if ok {
		return terms
	}

	rec, err := s.statsBucket.Get(ctx, docsStatsRecordID)
	if err != nil && !errors.Is(err, bw.ErrNotFound) {
		// Statistics are an optimisation; a read failure must not fail search.
		return nil
	}

	if rec != nil {
		terms = searchutil.NewStopWords(rec.Frequent)
	}

	s.stats.Fill(gen, terms)

	return terms
}

// RefreshStats recomputes the frequent-term set from the indexed documents when
// the corpus has changed enough to matter. It is local-only (no LLM or embedder
// calls), cheap when nothing changed, and safe to run in the background after an
// index rebuild.
func (s *TextStore) RefreshStats(ctx context.Context) error {
	count, err := s.bucket.Count(ctx, nil)
	if err != nil {
		return fmt.Errorf("count docs search chunks; %w", err)
	}

	total := int(count)
	if total == 0 {
		if err := s.statsBucket.Delete(ctx, docsStatsRecordID); err != nil && !errors.Is(err, bw.ErrNotFound) {
			return fmt.Errorf("clear docs search stats; %w", err)
		}
		s.stats.Invalidate()

		return nil
	}

	if prev, err := s.statsBucket.Get(ctx, docsStatsRecordID); err == nil && prev != nil && searchutil.StatsFresh(prev.Total, total) {
		return nil
	}

	// Sample evenly across the whole bucket. Keys are repo-prefixed, so taking
	// a prefix of the walk would measure one repository's vocabulary instead of
	// the corpus's.
	sampler := searchutil.NewFrequentTermSampler(total)

	if err := s.bucket.WalkSample(ctx, sampler.Stride(), func(record *textRecord) error {
		sampler.ObserveSample(record.Title, record.Excerpt)

		return nil
	}); err != nil {
		return fmt.Errorf("walk docs search chunks; %w", err)
	}

	frequent, sampled := sampler.Result()
	if sampled == 0 {
		return nil
	}

	rec := &textStats{
		ID:        docsStatsRecordID,
		Total:     total,
		Sampled:   sampled,
		Frequent:  frequent,
		UpdatedAt: time.Now(),
	}
	if err := s.statsBucket.Insert(ctx, rec); err != nil {
		return fmt.Errorf("save docs search stats; %w", err)
	}

	s.stats.Publish(searchutil.NewStopWords(frequent))

	slog.Info("docs search stats refreshed",
		"chunks", total, "sampled", sampled, "frequent_terms", len(frequent))

	return nil
}
