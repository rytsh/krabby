package rag

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// countingEmbedServer records the largest single request it is asked to embed,
// which is how a streaming index is distinguished from one that materialises
// the whole corpus first.
type countingEmbedServer struct {
	mu       sync.Mutex
	maxBatch int
	texts    int
}

func (c *countingEmbedServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		c.mu.Lock()
		c.maxBatch = max(c.maxBatch, len(req.Input))
		c.texts += len(req.Input)
		c.mu.Unlock()

		type datum struct {
			Embedding []float32 `json:"embedding"`
		}

		var resp struct {
			Data []datum `json:"data"`
		}

		for _, text := range req.Input {
			resp.Data = append(resp.Data, datum{Embedding: []float32{
				float32(len(text)) + 0.01,
				float32(strings.Count(text, "a")) + 0.01,
				0.01,
			}})
		}

		_ = json.NewEncoder(w).Encode(resp)
	}
}

func newCountingService(t *testing.T) (*Service, *countingEmbedServer) {
	t.Helper()

	counter := &countingEmbedServer{}
	srv := httptest.NewServer(counter.handler())
	t.Cleanup(srv.Close)

	// A batch far larger than chunkFlushSize, so any bound observed in the
	// requests comes from the indexer rather than the embedder client.
	emb, err := embedder.New(config.Embedder{
		BaseURL: srv.URL, Model: "fake", Batch: 4096, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("embedder.New: %v", err)
	}

	store, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("vectorstore.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.RAG{ChunkSize: 200, ChunkOverlap: 40, TopK: 20, TopDocs: 5}

	return New(cfg, emb, store), counter
}

// manyDocs writes n documents, each long enough to produce several chunks, so
// the corpus comfortably exceeds chunkFlushSize.
func manyDocs(t *testing.T, n int) string {
	t.Helper()

	files := make(map[string]string, n)
	for i := range n {
		files[fmt.Sprintf("doc-%03d.md", i)] = fmt.Sprintf(
			"# Document %d\n\n%s\n", i, strings.Repeat("alpha beta gamma delta ", 120))
	}

	return writeDocs(t, files)
}

// TestIndexStreamsInBoundedBatches is the regression test for the OOM this
// package caused: indexing used to build every chunk and every embedding of a
// collection in memory before writing anything, so a synced JIRA project of a
// few thousand issues sized the heap. Peak memory must now be a constant.
func TestIndexStreamsInBoundedBatches(t *testing.T) {
	svc, counter := newCountingService(t)
	docsDir := manyDocs(t, 200)

	if err := svc.Index(t.Context(), "repo", docsDir); err != nil {
		t.Fatalf("Index: %v", err)
	}

	counter.mu.Lock()
	maxBatch, texts := counter.maxBatch, counter.texts
	counter.mu.Unlock()

	if texts <= chunkFlushSize {
		t.Fatalf("corpus produced %d chunks, too few to exercise batching (need > %d)",
			texts, chunkFlushSize)
	}
	if maxBatch > chunkFlushSize {
		t.Errorf("largest embed request was %d chunks, want at most %d: "+
			"the whole corpus is still being buffered", maxBatch, chunkFlushSize)
	}
}

// TestIndexReportsDeterminateProgress guards the contract the streaming rewrite
// had to preserve: the total is known up front, so the UI shows a real
// progress bar rather than a number that grows as work is discovered.
func TestIndexReportsDeterminateProgress(t *testing.T) {
	svc, _ := newCountingService(t)
	docsDir := manyDocs(t, 200)

	var (
		totals []int
		lastfn int
	)

	err := svc.IndexProgress(t.Context(), "repo", docsDir, func(done, total int) {
		totals = append(totals, total)
		if done < lastfn {
			t.Errorf("progress went backwards: %d after %d", done, lastfn)
		}
		if done > total {
			t.Errorf("done %d exceeds total %d", done, total)
		}
		lastfn = done
	})
	if err != nil {
		t.Fatalf("IndexProgress: %v", err)
	}

	if len(totals) < 2 {
		t.Fatalf("progress reported %d times, expected several batches", len(totals))
	}
	for i, total := range totals {
		if total != totals[0] {
			t.Fatalf("total changed mid-run: report %d said %d, first said %d",
				i, total, totals[0])
		}
	}
	if lastfn != totals[0] {
		t.Errorf("final done = %d, want the full total %d", lastfn, totals[0])
	}
}

// TestIndexPathsStreamsInBoundedBatches covers the incremental path, which is
// what a full JIRA or Confluence resync actually takes: thousands of changed
// paths in one call.
func TestIndexPathsStreamsInBoundedBatches(t *testing.T) {
	svc, counter := newCountingService(t)
	docsDir := manyDocs(t, 200)

	changed := make([]string, 0, 200)
	for i := range 200 {
		changed = append(changed, fmt.Sprintf("doc-%03d.md", i))
	}

	if err := svc.IndexPaths(t.Context(), "repo", docsDir, changed, nil); err != nil {
		t.Fatalf("IndexPaths: %v", err)
	}

	counter.mu.Lock()
	maxBatch, texts := counter.maxBatch, counter.texts
	counter.mu.Unlock()

	if texts <= chunkFlushSize {
		t.Fatalf("corpus produced %d chunks, too few to exercise batching", texts)
	}
	if maxBatch > chunkFlushSize {
		t.Errorf("largest embed request was %d chunks, want at most %d",
			maxBatch, chunkFlushSize)
	}
}

// TestIndexedChunksAreRetrievable makes sure the streaming rewrite still stores
// every chunk: a batching bug that dropped the final partial batch would
// otherwise pass the bounds tests above unnoticed.
func TestIndexedChunksAreRetrievable(t *testing.T) {
	svc, counter := newCountingService(t)
	docsDir := manyDocs(t, 20)

	if err := svc.Index(t.Context(), "repo", docsDir); err != nil {
		t.Fatalf("Index: %v", err)
	}

	counter.mu.Lock()
	embedded := counter.texts
	counter.mu.Unlock()

	indexed, err := svc.store.IndexedPaths(t.Context(), "repo")
	if err != nil {
		t.Fatalf("IndexedPaths: %v", err)
	}

	if len(indexed) != 20 {
		t.Errorf("indexed %d documents, want 20", len(indexed))
	}
	if embedded == 0 {
		t.Error("no chunks were embedded")
	}
}
