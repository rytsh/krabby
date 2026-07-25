package coderag

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// batchCounter records the largest single embed request, which is how a
// streaming index is told apart from one that buffers the whole repository.
type batchCounter struct {
	mu       sync.Mutex
	maxBatch int
	texts    int
}

func (c *batchCounter) server(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		c.mu.Lock()
		c.maxBatch = max(c.maxBatch, len(req.Input))
		c.texts += len(req.Input)
		c.mu.Unlock()

		data := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			data[i] = map[string]any{"embedding": []float32{float32(len(in)), 1}}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)

	return srv
}

func (c *batchCounter) read() (maxBatch, texts int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.maxBatch, c.texts
}

// manySourceFiles writes n Go files, each large enough to yield several chunks
// at the configured chunk size, so the repository exceeds indexFlushChunks.
func manySourceFiles(t *testing.T, n int) string {
	t.Helper()

	root := t.TempDir()
	body := strings.Repeat("\t// padding to make this function long enough to chunk\n", 40)

	for i := range n {
		src := fmt.Sprintf("package pkg%d\n\nfunc Handler%d() {\n%s}\n", i, i, body)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file%03d.go", i)), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func newStreamService(t *testing.T, counter *batchCounter, text *TextStore) *Service {
	t.Helper()

	srv := counter.server(t)

	// A batch far above indexFlushChunks so any bound in the requests comes
	// from the indexer, not the embedder client.
	emb, err := embedder.New(config.Embedder{
		BaseURL: srv.URL, Model: "test", Batch: 4096, Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	store, err := vectorstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Small chunks so a modest tree produces plenty of them.
	cfg := config.CodeRAG{ChunkSize: 300, ChunkOverlap: 60, TopK: 1}

	return New(cfg, emb, store, nil, text)
}

func newTextStore(t *testing.T) *TextStore {
	t.Helper()

	db, err := bw.Open(t.TempDir(), bw.WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	text, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return text
}

// TestIndexStreamsInBoundedBatches is the regression test for the startup OOM:
// indexing used to build every chunk of a repository in memory before writing
// anything, so peak usage scaled with the largest tracked repository.
func TestIndexStreamsInBoundedBatches(t *testing.T) {
	t.Parallel()

	counter := &batchCounter{}
	svc := newStreamService(t, counter, nil)
	root := manySourceFiles(t, 120)

	if err := svc.Index(t.Context(), "acme/app", root); err != nil {
		t.Fatal(err)
	}

	maxBatch, texts := counter.read()
	if texts <= indexFlushChunks {
		t.Fatalf("repository produced %d chunks, too few to exercise batching (need > %d)",
			texts, indexFlushChunks)
	}
	if maxBatch > indexFlushChunks {
		t.Errorf("largest embed request was %d chunks, want at most %d: "+
			"the whole repository is still being buffered", maxBatch, indexFlushChunks)
	}
}

// TestIndexTextStreams covers the path the background warm pass runs for every
// tracked repository at startup, which is where the reported OOM happened.
func TestIndexTextStreams(t *testing.T) {
	t.Parallel()

	text := newTextStore(t)
	counter := &batchCounter{}
	svc := newStreamService(t, counter, text)
	root := manySourceFiles(t, 120)

	if err := svc.IndexText(t.Context(), "acme/app", root); err != nil {
		t.Fatal(err)
	}

	// IndexText builds no vectors, so the embedder must not be called at all.
	if _, texts := counter.read(); texts != 0 {
		t.Errorf("IndexText embedded %d chunks, want none", texts)
	}

	has, err := text.HasRepo(t.Context(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("no full-text chunks were indexed")
	}

	// A file from the middle of the tree, i.e. one that only a later batch can
	// have written: a drain bug that dropped batches would show up here.
	page, err := text.Search(t.Context(), "acme/app", "pkg60", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total == 0 {
		t.Error("a package from the middle of the repository is missing from the index")
	}
}

// TestIndexTextReplacesPriorChunks pins the semantics IndexText inherited from
// ReplaceRepo: a rebuild must not leave chunks of files that no longer exist.
func TestIndexTextReplacesPriorChunks(t *testing.T) {
	t.Parallel()

	text := newTextStore(t)
	counter := &batchCounter{}
	svc := newStreamService(t, counter, text)
	root := manySourceFiles(t, 4)

	if err := svc.IndexText(t.Context(), "acme/app", root); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "file003.go")); err != nil {
		t.Fatal(err)
	}

	if err := svc.IndexText(t.Context(), "acme/app", root); err != nil {
		t.Fatal(err)
	}

	page, err := text.Search(t.Context(), "acme/app", "Handler3", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range page.Results {
		if hit.Path == "file003.go" {
			t.Error("chunks of a deleted file survived the rebuild")
		}
	}
}

// TestIndexProgressReportsDeterminateProgress guards the contract the streaming
// rewrite had to preserve: the total is known before embedding starts.
func TestIndexProgressReportsDeterminateProgress(t *testing.T) {
	t.Parallel()

	counter := &batchCounter{}
	svc := newStreamService(t, counter, nil)
	root := manySourceFiles(t, 120)

	var (
		totals []int
		last   int
	)

	err := svc.IndexProgress(t.Context(), "acme/app", root, func(done, total int) {
		totals = append(totals, total)
		if done < last {
			t.Errorf("progress went backwards: %d after %d", done, last)
		}
		last = done
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(totals) < 2 {
		t.Fatalf("progress reported %d times, expected several batches", len(totals))
	}
	for i, total := range totals {
		if total != totals[0] {
			t.Fatalf("total changed mid-run: report %d said %d, first said %d", i, total, totals[0])
		}
	}
	if last != totals[0] {
		t.Errorf("final done = %d, want the full total %d", last, totals[0])
	}
}
