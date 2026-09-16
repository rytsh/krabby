package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/taskstore"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/storage"
	"github.com/rytsh/krabby/internal/transfer"
)

func TestImportedReaderServesQueriesWithoutWritesOrGraphify(t *testing.T) {
	ctx := context.Background()
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	source, target := t.TempDir(), t.TempDir()
	repoID := "example.org/team/repo"
	clone := filepath.Join(source, "repos", repoID)
	ok(os.MkdirAll(filepath.Join(clone, ".git"), 0o700))
	ok(os.WriteFile(filepath.Join(clone, "main.go"), []byte("package invoicing"), 0o600))
	docs := filepath.Join(source, "docs", repoID)
	ok(os.MkdirAll(docs, 0o700))
	ok(os.WriteFile(filepath.Join(docs, "doc.md"), []byte("# Invoicing\nTransactional invoices"), 0o600))
	db, err := storage.Open(filepath.Join(source, "state"))
	ok(err)
	r, err := registry.New(db)
	ok(err)
	ok(r.Upsert(ctx, &registry.Repo{ID: repoID, Path: clone, Status: registry.StatusReady}))
	_, err = coderag.NewTextStore(db)
	ok(err)
	text, err := rag.NewTextStore(db)
	ok(err)
	ok(text.Index(ctx, repoID, docs))
	_, err = credentials.New(db, filepath.Join(source, "keys"))
	ok(err)
	_, err = taskstore.New(db)
	ok(err)
	_, err = websource.New(db)
	ok(err)
	_, err = apicatalog.New(db)
	ok(err)
	seed := settings.Defaults()
	seed.DocsEnabled, seed.RAGEnabled, seed.CodeRAGEnabled = true, true, true
	seed.EmbedBaseURL, seed.EmbedModel, seed.EmbedDim = "http://publisher.invalid", "test-embedding", 3
	_, err = settings.New(db, seed)
	ok(err)
	ok(db.Close())
	for _, name := range []string{"docs-vectors", "code-vectors"} {
		store, err := vectorstore.New(filepath.Join(source, name))
		ok(err)
		ok(store.Upsert(ctx, []vectorstore.Item{{ID: "one", Vector: []float32{1, 0, 0}, Payload: vectorstore.Payload{Repo: repoID, DocPath: "doc.md", Title: "Invoicing", Chunk: "Transactional invoices"}}}))
		ok(store.Close())
	}
	archive := filepath.Join(t.TempDir(), "full.krabby")
	status, err := transfer.Export(ctx, source, archive, config.Version, nil)
	ok(err)
	_, err = transfer.Import(ctx, target, archive, config.Version)
	ok(err)
	var embeddings atomic.Int32
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "test-embedding" {
			http.Error(w, "unexpected embedding request", http.StatusBadRequest)
			return
		}
		embeddings.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0,0]}]}`)
	}))
	defer embedServer.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	ok(err)
	_, port, err := net.SplitHostPort(listener.Addr().String())
	ok(err)
	ok(listener.Close())
	cfg := &config.Config{
		DataDir: target, ReadOnly: true,
		Server:   config.Server{Host: "127.0.0.1", Port: port, BasePath: "/krabby"},
		MCP:      config.MCP{Path: "/mcp"},
		Graphify: config.Graphify{Bin: "/nonexistent/graphify", Python: "/nonexistent/python"},
		Memory:   config.Memory{LimitBytes: 512 << 20, Ratio: 0.75},
		Query:    config.QueryConnections{DocsEmbedder: config.QueryEndpoint{BaseURL: embedServer.URL}},
	}
	serverCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serve(serverCtx, cfg) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			ok(err)
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	}()
	base := "http://127.0.0.1:" + port + "/krabby"
	client := &http.Client{Timeout: 3 * time.Second}
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		res, err := client.Get(base + "/healthz")
		if err == nil {
			_ = res.Body.Close()
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("read-only server did not start")
	}
	for _, route := range []string{
		"/api/v1/repos/" + repoID + "/-/file?path=main.go",
		"/api/v1/docs/search?q=invoicing&mode=lexical",
		"/api/v1/docs/search?q=invoicing&mode=semantic",
		"/api/v1/code/search?q=invoicing&mode=semantic",
	} {
		res, err := client.Get(base + route)
		ok(err)
		data, err := io.ReadAll(res.Body)
		ok(err)
		ok(res.Body.Close())
		if res.StatusCode != http.StatusOK || (!strings.Contains(strings.ToLower(string(data)), "invoic")) {
			t.Fatalf("%s: %d %s", route, res.StatusCode, data)
		}
	}
	if embeddings.Load() != 2 {
		t.Fatalf("embedding calls=%d, want query-only docs+code calls", embeddings.Load())
	}
	res, err := client.Post(base+"/api/v1/repos", "application/json", strings.NewReader(`{"url":"https://example.org/new"}`))
	ok(err)
	ok(res.Body.Close())
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation status=%d", res.StatusCode)
	}
	res, err = client.Get(base + "/api/v1/sync/status")
	ok(err)
	var got transfer.Status
	ok(json.NewDecoder(res.Body).Decode(&got))
	ok(res.Body.Close())
	if got.ID != status.ID {
		t.Fatalf("status=%+v", got)
	}
	// Startup and queries must preserve all three replicated version clocks.
	a, err := transfer.Active(target)
	ok(err)
	for name, version := range status.Versions {
		db, err := storage.OpenReadOnly(filepath.Join(a.Directory, name))
		ok(err)
		if got := db.Badger().MaxVersion(); got != version {
			t.Errorf("%s version changed: %d -> %d", name, version, got)
		}
		ok(db.Close())
	}
}
