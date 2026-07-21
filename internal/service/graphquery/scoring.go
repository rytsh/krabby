package graphquery

import (
	"math"
	"sort"
	"strings"
)

// Match bonuses mirror serve.py's three-tier scoring precedence.
const (
	exactMatchBonus     = 1000.0
	prefixMatchBonus    = 100.0
	substringMatchBonus = 1.0
	sourceMatchBonus    = 0.5
)

// computeIDF returns IDF weights for query terms, memoised on the graph so common
// terms ("error") get low weight and rare identifiers get high weight (mirrors
// _compute_idf). The cache lives on the Graph and is discarded on reload.
func (g *Graph) computeIDF(terms []string) map[string]float64 {
	n := g.NumNodes()
	if n == 0 {
		n = 1
	}

	var uncached []string
	for _, t := range terms {
		if _, ok := g.idfCache[t]; !ok {
			uncached = append(uncached, t)
		}
	}

	if len(uncached) > 0 {
		df := make(map[string]int, len(uncached))
		for _, t := range uncached {
			df[t] = 0
		}

		for _, id := range g.nodeList {
			nl := normLabelOf(g.Nodes[id])
			for _, t := range uncached {
				if strings.Contains(nl, t) {
					df[t]++
				}
			}
		}

		for _, t := range uncached {
			g.idfCache[t] = math.Log(1 + float64(n)/float64(1+df[t]))
		}
	}

	out := make(map[string]float64, len(terms))
	fallback := math.Log(1 + float64(n))
	for _, t := range terms {
		if v, ok := g.idfCache[t]; ok {
			out[t] = v
		} else {
			out[t] = fallback
		}
	}

	return out
}

// scoredNode is a (score, nodeID) pair.
type scoredNode struct {
	score float64
	id    string
}

// scoreNodes ranks nodes for a set of query terms (mirrors _score_nodes). Terms
// are re-tokenised through searchTokens first. Results are sorted by score desc,
// then by node id (stable, deterministic) — python sorts tuples so ties fall back
// to node id, which we replicate.
func (g *Graph) scoreNodes(terms []string) []scoredNode {
	var normTerms []string
	for _, t := range terms {
		normTerms = append(normTerms, searchTokens(t)...)
	}

	idf := g.computeIDF(normTerms)

	var scored []scoredNode
	for _, id := range g.nodeList {
		d := g.Nodes[id]
		normLabel := normLabelOf(d)
		bareLabel := strings.TrimRight(normLabel, "()")
		source := strings.ToLower(d.SourceFile)
		score := 0.0

		for _, t := range normTerms {
			w := idf[t]
			if w == 0 {
				w = 1.0
			}

			switch {
			case t == normLabel || t == bareLabel:
				score += exactMatchBonus * w
			case strings.HasPrefix(normLabel, t) || strings.HasPrefix(bareLabel, t):
				score += prefixMatchBonus * w
			case strings.Contains(normLabel, t):
				score += substringMatchBonus * w
			}

			if source != "" && strings.Contains(source, t) {
				score += sourceMatchBonus * w
			}
		}

		if score > 0 {
			scored = append(scored, scoredNode{score: score, id: id})
		}
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		// Python sorts (score, id) tuples reverse=True, so higher id wins ties.
		return scored[i].id > scored[j].id
	})

	return scored
}

// pickSeeds selects BFS/DFS seed nodes, stopping when score drops too far below
// the top (mirrors _pick_seeds: maxK=3, gapRatio=0.2).
func pickSeeds(scored []scoredNode) []string {
	const (
		maxK     = 3
		gapRatio = 0.2
	)

	if len(scored) == 0 {
		return nil
	}

	top := scored[0].score
	var seeds []string
	limit := min(maxK, len(scored))
	for i := range limit {
		if len(seeds) > 0 && scored[i].score < top*gapRatio {
			break
		}
		seeds = append(seeds, scored[i].id)
	}

	return seeds
}

// findNode returns node ids whose label/id matches the term, ordered by
// exact > prefix > substring precedence (mirrors _find_node).
func (g *Graph) findNode(label string) []string {
	term := strings.Join(searchTokens(label), " ")
	if term == "" {
		return nil
	}

	var exact, prefix, substring []string
	for _, id := range g.nodeList {
		d := g.Nodes[id]
		normLabel := normLabelOf(d)
		bareLabel := strings.TrimRight(normLabel, "()")
		idLower := strings.ToLower(id)

		switch {
		case term == normLabel || term == bareLabel || term == idLower:
			exact = append(exact, id)
		case strings.HasPrefix(normLabel, term) || strings.HasPrefix(bareLabel, term) || strings.HasPrefix(idLower, term):
			prefix = append(prefix, id)
		case strings.Contains(normLabel, term):
			substring = append(substring, id)
		}
	}

	return append(append(exact, prefix...), substring...)
}
