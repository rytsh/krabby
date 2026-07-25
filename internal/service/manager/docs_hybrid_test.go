package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// slowEmbedServer is an embeddings endpoint that takes a known amount of time,
// so a test can give the semantic ranker a controlled cost.
func slowEmbedServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		time.Sleep(delay)

		type datum struct {
			Embedding []float32 `json:"embedding"`
		}
		var resp struct {
			Data []datum `json:"data"`
		}
		for _, text := range req.Input {
			lower := strings.ToLower(text)
			resp.Data = append(resp.Data, datum{Embedding: []float32{
				float32(strings.Count(lower, "gateway")) + 0.01,
				float32(strings.Count(lower, "timeout")) + 0.01,
			}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// hybridFixture wires a manager with both rankers live. The two indexes are
// built from different directories on purpose: the vector index stays tiny (it
// is priced by the stubbed embedder's delay) while the text index is given a
// corpus large enough for the BM25 arm to cost something measurable.
type hybridFixture struct {
	m      *Manager
	repoID string
}

func newHybridFixture(t *testing.T, embedDelay time.Duration, lexicalChunks int) hybridFixture {
	t.Helper()
	ctx := context.Background()

	emb, err := embedder.New(config.Embedder{BaseURL: slowEmbedServer(t, embedDelay).URL, Model: "fake"})
	if err != nil {
		t.Fatal(err)
	}

	store, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

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

	repo := &registry.Repo{ID: "acme/payments"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	// Small directory for the vector index.
	docsRoot := t.TempDir()
	smallDir := filepath.Join(docsRoot, "acme", "payments")
	mustWriteManagerTest(t, filepath.Join(smallDir, "incident.md"),
		"# Gateway timeout\n\nThe payment gateway timed out during capture.")

	// Large directory for the text index: every chunk shares the query's
	// vocabulary, which is the shape that makes BM25 expensive.
	bigDir := t.TempDir()
	const chunksPerFile = 400
	body := strings.Repeat("The payment gateway timed out during capture and was retried. ", 20)
	for file := 0; file < lexicalChunks/chunksPerFile; file++ {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("# Gateway timeout report %d\n\n", file))
		for i := range chunksPerFile {
			sb.WriteString(fmt.Sprintf("## Incident %d-%d\n\n%s\n\n", file, i, body))
		}
		mustWriteManagerTest(t, filepath.Join(bigDir, fmt.Sprintf("report-%d.md", file)), sb.String())
	}

	ragSvc := rag.New(config.RAG{ChunkSize: 400, ChunkOverlap: 40, TopK: 10, TopDocs: 5}, emb, store)
	if err := ragSvc.Index(ctx, repo.ID, smallDir); err != nil {
		t.Fatal(err)
	}
	if err := text.Index(ctx, repo.ID, bigDir); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		reg:         reg,
		docsText:    text,
		docsRootDir: docsRoot,
		locks:       map[string]*sync.Mutex{},
		docs:        &docsBundle{rag: ragSvc, store: store},
	}
	m.docsTextWarmed.Store(true)

	return hybridFixture{m: m, repoID: repo.ID}
}

// TestHybridRunsBothRankersConcurrently pins the property the hybrid arms were
// made concurrent for: its latency is the slower ranker, not the sum of both.
//
// The assertion is relative to this machine — each ranker is timed on its own
// first — and is skipped when the arms turn out too cheap to tell the two
// implementations apart, so it reports a real regression or nothing.
func TestHybridRunsBothRankersConcurrently(t *testing.T) {
	const (
		embedDelay = 200 * time.Millisecond
		// The smaller arm bounds the saving, so it has to be worth measuring.
		minLexical = 15 * time.Millisecond
	)

	ctx := context.Background()
	f := newHybridFixture(t, embedDelay, 3200)

	search := func(mode string) time.Duration {
		t.Helper()

		started := time.Now()
		docs, err := f.m.SearchDocs(ctx, ScopeRepos, f.repoID, registry.NamespaceAll, mode, "payment gateway timeout capture", 5)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if len(docs) == 0 {
			t.Fatalf("%s returned nothing; a search that did no work times nothing", mode)
		}

		return time.Since(started)
	}

	semanticTook := search(DocsSearchSemantic)
	lexicalTook := search(DocsSearchLexical)
	hybridTook := search(DocsSearchHybrid)
	t.Logf("semantic %v lexical %v hybrid %v", semanticTook, lexicalTook, hybridTook)

	if lexicalTook < minLexical {
		t.Skipf("lexical arm too cheap to measure overlap (%v); the timing says nothing", lexicalTook)
	}

	// Sequential would cost both arms. Concurrent costs the slower one, so
	// anything below the sum minus half the smaller arm can only be overlap.
	smaller := min(semanticTook, lexicalTook)
	limit := semanticTook + lexicalTook - smaller/2

	if hybridTook >= limit {
		t.Fatalf("hybrid took %v, want < %v (semantic %v + lexical %v): the rankers did not overlap",
			hybridTook, limit, semanticTook, lexicalTook)
	}
}

// TestHybridReportsRankerFailure keeps the error contract: hybrid needs both
// indexes and must not quietly return half an answer.
func TestHybridReportsRankerFailure(t *testing.T) {
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
	repo := &registry.Repo{ID: "acme/payments"}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	docsDir := filepath.Join(docsRoot, "acme", "payments")
	mustWriteManagerTest(t, filepath.Join(docsDir, "incident.md"), "# PAY-1\n\ngateway timeout during capture")
	if err := text.Index(ctx, repo.ID, docsDir); err != nil {
		t.Fatal(err)
	}

	// No rag service: the semantic arm cannot run.
	m := &Manager{
		reg:         reg,
		docsText:    text,
		docsRootDir: docsRoot,
		locks:       map[string]*sync.Mutex{},
		docs:        &docsBundle{},
	}
	m.docsTextWarmed.Store(true)

	_, err = m.SearchDocs(ctx, ScopeRepos, repo.ID, registry.NamespaceAll, DocsSearchHybrid, "gateway timeout", 5)
	if err == nil || !strings.Contains(err.Error(), "semantic docs search is not enabled") {
		t.Fatalf("hybrid without a semantic index = %v, want the semantic failure", err)
	}

	// The surviving ranker still answers on its own.
	docs, err := m.SearchDocs(ctx, ScopeRepos, repo.ID, registry.NamespaceAll, DocsSearchLexical, "gateway timeout", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("lexical docs = %#v", docs)
	}
}

// TestLogDocsSearchAttributes guards the slow-query log's key/value pairing,
// which slog would panic on if an attribute were added without its value.
func TestLogDocsSearchAttributes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		total, semantic, lexical time.Duration
	}{
		{total: time.Millisecond},
		{total: docsSearchSlowThreshold, semantic: 400 * time.Millisecond, lexical: 100 * time.Millisecond},
		{total: 2 * docsSearchSlowThreshold, lexical: 2 * docsSearchSlowThreshold},
	} {
		logDocsSearch(DocsSearchHybrid, ScopeAll, "web:jira", tc.total, tc.semantic, tc.lexical, 3)
		logDocsSearch(DocsSearchLexical, ScopeAll, "", tc.total, 0, tc.lexical, 0)
	}
}
