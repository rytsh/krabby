package manager

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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

		bundleState: bundleState{docs: &docsBundle{}},
	}
	page, err := m.SearchDocs(ctx, ScopeRepos, repo.ID, "", DocsSearchLexical, "PAY-1842", 5)
	docs := page.Results
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
	page, err = m.SearchDocs(ctx, ScopeRepos, repo.ID, "", "", "PAY-1842", 5)
	docs = page.Results
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

	withEmbedder := &Manager{bundleState: bundleState{docs: &docsBundle{rag: &rag.Service{}}}}
	if got := withEmbedder.resolveDocsSearchMode(""); got != DocsSearchSemantic {
		t.Fatalf("default with a semantic index = %q, want semantic", got)
	}
	for _, mode := range []string{DocsSearchHybrid, DocsSearchLexical, DocsSearchSemantic} {
		if got := withEmbedder.resolveDocsSearchMode(mode); got != mode {
			t.Fatalf("explicit %q was overridden with %q", mode, got)
		}
	}

	withoutEmbedder := &Manager{bundleState: bundleState{docs: &docsBundle{}}}
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

		bundleState: bundleState{docs: &docsBundle{}},
	}

	if err := m.WarmDocsSearch(ctx); err != nil {
		t.Fatal(err)
	}

	frequent := text.FrequentTerms(ctx)
	if !frequent["payment"] || !frequent["gateway"] {
		t.Fatalf("test premise broken: %q/%q are not corpus-wide here: %#v", "payment", "gateway", frequent)
	}

	page, err := m.SearchDocs(ctx, ScopeRepos, repo.ID, "", DocsSearchLexical, "payment gateway", 5)
	docs := page.Results
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatal("frequent-term filtering swallowed every result instead of retrying unfiltered")
	}
}
