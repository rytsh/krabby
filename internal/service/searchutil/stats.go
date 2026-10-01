package searchutil

import (
	"math"
	"slices"

	"github.com/rakunlabs/bw"
)

const (
	// frequentTermRatio is the document-frequency share above which a term is
	// dropped from a built lexical query.
	//
	// Query time is linear in the number of documents a term matches (bw reads
	// each matching document's length to score it), so a term present in most
	// of the corpus dominates the cost of a whole question. Such a term also
	// carries almost no ranking signal: BM25's IDF, ln(1+(N-df+0.5)/(df+0.5)),
	// gives a term at this share under a quarter of a rare term's weight. The
	// threshold is set high enough that it catches genuine function words
	// (which sit at 0.6-1.0) while leaving domain vocabulary searchable.
	frequentTermRatio = 0.35

	// statsSampleSize bounds how many documents are inspected when estimating
	// document frequency. Only very common terms matter here, and their share
	// is estimated from a sample this size with well under a percent of error,
	// so a full pass would cost time without changing the outcome.
	statsSampleSize = 2000

	// maxFrequentTerms caps the persisted set. Corpora that are genuinely
	// repetitive would otherwise store a large and useless list.
	maxFrequentTerms = 500

	// statsStaleRatio is how much the corpus must grow or shrink before the
	// frequent-term set is recomputed. Which terms are corpus-wide is stable
	// under small changes, so this keeps RefreshStats safe to call after every
	// index without walking the bucket each time.
	statsStaleRatio = 0.25
)

// ---------------------------------------------------------------------------
// Reusable sampler
// ---------------------------------------------------------------------------

// FrequentTermSampler accumulates document frequencies over a strided sample
// of a corpus and reports the terms too common to be worth searching.
//
// The bw plumbing around it — which bucket holds the record, which fields a
// document contributes — is per-store and stays with each store. The rule for
// deciding which terms are useless is not: it is the same threshold, the same
// sample size and the same cap whether the documents are Markdown pages or
// source chunks, and having two copies of it would let them drift.
type FrequentTermSampler struct {
	stride  int
	seen    int
	sampled int
	counts  map[string]int
	terms   map[string]struct{}
}

// NewFrequentTermSampler prepares a sampler for a corpus of total documents.
// Documents must be offered in the corpus's own key order: the stride is what
// keeps the sample spread across every partition rather than measuring the
// vocabulary of whichever repository sorts first.
func NewFrequentTermSampler(total int) *FrequentTermSampler {
	stride := total / statsSampleSize
	if stride < 1 {
		stride = 1
	}

	return &FrequentTermSampler{
		stride: stride,
		counts: make(map[string]int),
		terms:  make(map[string]struct{}),
	}
}

// Observe offers one document's searchable fields. Every stride-th document is
// tokenised; the rest only advance the counter.
func (s *FrequentTermSampler) Observe(fields ...string) {
	defer func() { s.seen++ }()

	if s.seen%s.stride != 0 {
		return
	}
	s.ObserveSample(fields...)
}

// Stride is the record interval used by the sampler. Stores can apply it to
// keys before decoding values and offer just the selected records below.
func (s *FrequentTermSampler) Stride() int { return s.stride }

// ObserveSample offers a record already selected at Stride intervals.
func (s *FrequentTermSampler) ObserveSample(fields ...string) {
	s.sampled++

	// Count each term once per document: document frequency, not term
	// frequency, is what drives both query cost and IDF.
	clear(s.terms)
	for _, field := range fields {
		for _, token := range statsTokenizer.Tokenize(field) {
			s.terms[token] = struct{}{}
		}
	}
	for term := range s.terms {
		s.counts[term]++
	}
}

// Result returns the sorted frequent terms and how many documents were
// inspected. A zero sample yields no terms, which simply means no filtering.
func (s *FrequentTermSampler) Result() (frequent []string, sampled int) {
	if s.sampled == 0 {
		return nil, 0
	}

	minDocs := int(frequentTermRatio * float64(s.sampled))
	frequent = make([]string, 0, 64)
	for term, df := range s.counts {
		if df >= minDocs {
			frequent = append(frequent, term)
		}
	}

	// Keep the most common ones when the corpus is unusually repetitive.
	if len(frequent) > maxFrequentTerms {
		slices.SortFunc(frequent, func(a, b string) int { return s.counts[b] - s.counts[a] })
		frequent = frequent[:maxFrequentTerms]
	}
	slices.Sort(frequent)

	return frequent, s.sampled
}

// StatsFresh reports whether a frequent-term set computed over prevTotal
// documents is still close enough to a corpus of total documents to keep.
// Which terms are corpus-wide is stable under small changes, so this is what
// makes a refresh safe to call after every index without walking the bucket.
func StatsFresh(prevTotal, total int) bool {
	if prevTotal <= 0 {
		return false
	}

	return math.Abs(float64(total-prevTotal))/float64(prevTotal) < statsStaleRatio
}

// statsTokenizer must match the tokenizer bw indexes with, otherwise the
// measured terms would not be the ones a query is evaluated against.
var statsTokenizer = bw.DefaultTokenizer{MinLen: 1}
