package docsearch

import (
	"sort"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/rag"
)

// Hybrid rank-fusion defaults, applied when the corresponding setting is zero
// (including records written before the settings existed).
const (
	defaultHybridCandidates = 12
	// defaultHybridRRFK is deliberately far below the classic RRF constant of
	// 60. That value was tuned for thousand-deep TREC runs; over a list of
	// ~12 candidates it compresses rank 1 and rank 12 to within 18% of each
	// other, which makes "appears in both lists" outweigh rank quality
	// entirely.
	defaultHybridRRFK    = 20
	defaultHybridWeight  = 1.0
	maxHybridCandidates  = rag.MaxCandidates
	minHybridCandidates  = 1
	minHybridRRFKAllowed = 0
)

// fuseParams tunes reciprocal rank fusion. Zero fields take the defaults.
type fuseParams struct {
	K          int
	WLex, WSem float64
}

func fuseParamsFor(cfg config.RAG) fuseParams {
	return fuseParams{
		K:    cfg.HybridRRFK,
		WLex: cfg.HybridWeightLexical,
		WSem: cfg.HybridWeightSemantic,
	}
}

func (p fuseParams) normalized() fuseParams {
	if p.K <= minHybridRRFKAllowed {
		p.K = defaultHybridRRFK
	}
	if p.WLex <= 0 {
		p.WLex = defaultHybridWeight
	}
	if p.WSem <= 0 {
		p.WSem = defaultHybridWeight
	}

	return p
}

// hybridCandidates is how many documents each ranker contributes to fusion.
func hybridCandidates(cfg config.RAG) int {
	n := cfg.HybridCandidates
	if n <= 0 {
		n = defaultHybridCandidates
	}
	if n < minHybridCandidates {
		n = minHybridCandidates
	}
	if n > maxHybridCandidates {
		n = maxHybridCandidates
	}

	return n
}

// fuseDocs combines BM25 and semantic ranks with weighted reciprocal rank
// fusion, which avoids comparing the two rankers' unrelated raw score scales.
//
// Both lists must be fetched at the same depth: a ranker that contributes more
// ranks also contributes more total fused score, so an asymmetric depth is an
// implicit weight. Use p.WLex/p.WSem to weight a ranker on purpose instead.
//
// Ties are broken on repo+path so the result is independent of map iteration
// and of which list happened to be scanned first. The excerpt of the
// better-ranked occurrence is kept, so a document found by both rankers still
// shows the text that matched exactly.
func fuseDocs(lexical, semantic []rag.Doc, topDocs int, p fuseParams) []rag.Doc {
	p = p.normalized()

	type fusedDoc struct {
		key      string
		doc      rag.Doc
		score    float64
		bestRank int
	}

	byKey := map[string]*fusedDoc{}

	for _, ranking := range []struct {
		docs   []rag.Doc
		weight float64
	}{
		{lexical, p.WLex},
		{semantic, p.WSem},
	} {
		for i, doc := range ranking.docs {
			key := doc.Repo + "\x00" + doc.Path
			entry := byKey[key]
			if entry == nil {
				entry = &fusedDoc{key: key, doc: doc, bestRank: i}
				byKey[key] = entry
			} else if i < entry.bestRank {
				entry.doc = doc
				entry.bestRank = i
			}
			entry.score += ranking.weight / float64(p.K+i+1)
		}
	}

	fused := make([]*fusedDoc, 0, len(byKey))
	for _, entry := range byKey {
		fused = append(fused, entry)
	}
	sort.Slice(fused, func(i, j int) bool {
		if fused[i].score == fused[j].score {
			return fused[i].key < fused[j].key
		}

		return fused[i].score > fused[j].score
	})

	if len(fused) > topDocs {
		fused = fused[:topDocs]
	}
	docs := make([]rag.Doc, 0, len(fused))
	for _, entry := range fused {
		entry.doc.Score = float32(entry.score)
		docs = append(docs, entry.doc)
	}

	return docs
}
