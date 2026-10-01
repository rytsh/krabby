package docsearch

import (
	"fmt"
	"testing"

	"github.com/rytsh/krabby/internal/service/rag"
)

func TestFuseDocsUsesRanksAndDeduplicates(t *testing.T) {
	t.Parallel()

	lexical := []rag.Doc{
		{Repo: "web:jira", Path: "pay-1.md", Excerpt: "exact PAY-1 match", Score: 12.4},
		{Repo: "web:jira", Path: "pay-2.md", Excerpt: "lexical second", Score: 8.1},
	}
	semantic := []rag.Doc{
		{Repo: "web:jira", Path: "pay-1.md", Excerpt: "conceptual match", Score: 0.91},
		{Repo: "web:jira", Path: "pay-3.md", Excerpt: "semantic second", Score: 0.85},
	}

	docs := fuseDocs(lexical, semantic, 3, fuseParams{})
	if len(docs) != 3 {
		t.Fatalf("fused docs = %#v", docs)
	}
	if docs[0].Path != "pay-1.md" {
		t.Fatalf("shared top result did not win: %#v", docs)
	}
	if docs[0].Excerpt != "exact PAY-1 match" {
		t.Fatalf("equal-rank tie did not keep lexical excerpt: %#v", docs[0])
	}
	if docs[0].Score <= docs[1].Score {
		t.Fatalf("fused score did not reward both rankings: %#v", docs)
	}
}

// TestFuseDocsWeightsApply checks that a ranker is weighted only on purpose.
func TestFuseDocsWeightsApply(t *testing.T) {
	t.Parallel()

	lexical := []rag.Doc{{Repo: "r", Path: "lex.md"}}
	semantic := []rag.Doc{{Repo: "r", Path: "sem.md"}}

	equal := fuseDocs(lexical, semantic, 2, fuseParams{})
	if equal[0].Score != equal[1].Score {
		t.Fatalf("equal weights did not tie rank 1 against rank 1: %#v", equal)
	}
	if equal[0].Path != "lex.md" {
		t.Fatalf("tie must break deterministically on repo+path: %#v", equal)
	}

	leaning := fuseDocs(lexical, semantic, 2, fuseParams{WLex: 0.5, WSem: 1})
	if leaning[0].Path != "sem.md" {
		t.Fatalf("down-weighted lexical still won: %#v", leaning)
	}
}

// TestFuseDocsDepthIsNotAnImplicitWeight pins the regression this fusion was
// rewritten for: the semantic arm used to be capped at rag.MaxTopDocs while the
// lexical arm returned the full fetch depth, so lexical injected ~2.3x more
// fused score than semantic without anyone choosing that.
func TestFuseDocsDepthIsNotAnImplicitWeight(t *testing.T) {
	t.Parallel()

	mass := func(docs []rag.Doc) float64 {
		fused := fuseDocs(docs, nil, len(docs), fuseParams{})

		var total float64
		for _, doc := range fused {
			total += float64(doc.Score)
		}

		return total
	}

	list := func(prefix string, n int) []rag.Doc {
		docs := make([]rag.Doc, 0, n)
		for i := range n {
			docs = append(docs, rag.Doc{Repo: "r", Path: fmt.Sprintf("%s-%d.md", prefix, i)})
		}

		return docs
	}

	deep, shallow := mass(list("a", 12)), mass(list("b", 5))
	if ratio := deep / shallow; ratio < 1.5 {
		t.Fatalf("depth ratio %.2f: the test no longer exercises the imbalance", ratio)
	}

	// The guard against it is that SearchDocs asks both rankers for the same
	// depth, so equal depth must produce equal mass.
	if a, b := mass(list("a", 12)), mass(list("b", 12)); a != b {
		t.Fatalf("equal depth produced unequal fused mass: %v vs %v", a, b)
	}
}

// TestFuseDocsRRFKSpreadsShortLists guards the rank-1 vs rank-last spread. The
// classic k=60 flattens a 12-deep list to within ~18%, which lets a junk hit at
// the bottom of one list rival the best hit of the other.
func TestFuseDocsRRFKSpreadsShortLists(t *testing.T) {
	t.Parallel()

	docs := make([]rag.Doc, 0, defaultHybridCandidates)
	for i := range defaultHybridCandidates {
		docs = append(docs, rag.Doc{Repo: "r", Path: fmt.Sprintf("%d.md", i)})
	}

	fused := fuseDocs(docs, nil, len(docs), fuseParams{})
	spread := float64(fused[0].Score) / float64(fused[len(fused)-1].Score)

	if spread < 1.5 {
		t.Fatalf("rank-1/rank-last spread %.2f is too flat; k=%d compresses the list", spread, defaultHybridRRFK)
	}
}
