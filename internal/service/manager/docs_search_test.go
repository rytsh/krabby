package manager

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
)

func TestNormalizeDocsSearchMode(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		in, want string
		wantErr  bool
	}{
		// An unset mode stays unset here; the default is resolved per request
		// against what the installation has configured.
		{want: ""},
		{in: "  ", want: ""},
		{in: "HYBRID", want: DocsSearchHybrid},
		{in: DocsSearchSemantic, want: DocsSearchSemantic},
		{in: DocsSearchLexical, want: DocsSearchLexical},
		{in: "normal", wantErr: true},
	} {
		got, err := NormalizeDocsSearchMode(tt.in)
		if (err != nil) != tt.wantErr {
			t.Fatalf("NormalizeDocsSearchMode(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("NormalizeDocsSearchMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

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

func TestSearchDocsLexicalWithoutSemanticIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	text, err := rag.NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	docsRoot := t.TempDir()
	mustWriteManagerTest(t, filepath.Join(docsRoot, "acme", "payments", "incident.md"), "# PAY-1842\n\nGateway timeout ERR_CAPTURE_42")
	repo := &registry.Repo{ID: "acme/payments"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		reg:         reg,
		docsText:    text,
		docsRootDir: docsRoot,
		locks:       map[string]*sync.Mutex{},
		docs:        &docsBundle{},
	}
	docs, err := m.SearchDocs(ctx, ScopeRepos, repo.ID, "", DocsSearchLexical, "PAY-1842", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "incident.md" {
		t.Fatalf("lexical docs = %#v", docs)
	}

	_, err = m.SearchDocs(ctx, ScopeRepos, repo.ID, "", DocsSearchHybrid, "PAY-1842", 5)
	if err == nil || !strings.Contains(err.Error(), "semantic docs search is not enabled") {
		t.Fatalf("hybrid without semantic index error = %v", err)
	}

	// With no embedder configured the default must resolve to lexical: it is
	// the only mode this installation can serve.
	docs, err = m.SearchDocs(ctx, ScopeRepos, repo.ID, "", "", "PAY-1842", 5)
	if err != nil {
		t.Fatalf("default mode without an embedder: %v", err)
	}
	if len(docs) != 1 || docs[0].Path != "incident.md" {
		t.Fatalf("default mode docs = %#v", docs)
	}
}

// TestResolveDocsSearchModeDefaults pins the default: semantic when the
// installation can serve it, lexical otherwise, and an explicit mode always
// wins.
func TestResolveDocsSearchModeDefaults(t *testing.T) {
	t.Parallel()

	withEmbedder := &Manager{docs: &docsBundle{rag: &rag.Service{}}}
	if got := withEmbedder.resolveDocsSearchMode(""); got != DocsSearchSemantic {
		t.Fatalf("default with a semantic index = %q, want semantic", got)
	}
	for _, mode := range []string{DocsSearchHybrid, DocsSearchLexical, DocsSearchSemantic} {
		if got := withEmbedder.resolveDocsSearchMode(mode); got != mode {
			t.Fatalf("explicit %q was overridden with %q", mode, got)
		}
	}

	withoutEmbedder := &Manager{docs: &docsBundle{}}
	if got := withoutEmbedder.resolveDocsSearchMode(""); got != DocsSearchLexical {
		t.Fatalf("default without a semantic index = %q, want lexical", got)
	}
}

// TestSearchDocsRetriesWithoutFrequentTermFilter checks that dropping
// corpus-wide terms is only ever an optimisation. On a corpus narrow enough
// that every word of the question is common, the filtered query matches
// nothing and the unfiltered one must still be tried.
func TestSearchDocsRetriesWithoutFrequentTermFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	text, err := rag.NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	// Every document is about the payment gateway, so both words are in 100%
	// of the corpus and the frequent-term filter removes the whole question.
	docsRoot := t.TempDir()
	for i := range 20 {
		mustWriteManagerTest(t,
			filepath.Join(docsRoot, "acme", "payments", fmt.Sprintf("doc%d.md", i)),
			fmt.Sprintf("# Payment gateway %d\n\nThe payment gateway retries request %d.", i, i))
	}

	repo := &registry.Repo{ID: "acme/payments"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		reg:         reg,
		docsText:    text,
		docsRootDir: docsRoot,
		locks:       map[string]*sync.Mutex{},
		docs:        &docsBundle{},
	}

	if err := m.WarmDocsSearch(ctx); err != nil {
		t.Fatal(err)
	}

	frequent := text.FrequentTerms(ctx)
	if !frequent["payment"] || !frequent["gateway"] {
		t.Fatalf("test premise broken: %q/%q are not corpus-wide here: %#v", "payment", "gateway", frequent)
	}

	docs, err := m.SearchDocs(ctx, ScopeRepos, repo.ID, "", DocsSearchLexical, "payment gateway", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatal("frequent-term filtering swallowed every result instead of retrying unfiltered")
	}
}
