package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/storage"
)

func TestBigPictureRESTLifecycle(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	connections, err := mcpclient.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connections.Save(ctx, "", mcpclient.Config{Name: "config", URL: "https://config.example/mcp", BearerToken: "secret-token"}); err != nil {
		t.Fatal(err)
	}
	pictures, err := bigpicture.New(db, filepath.Join(dir, "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := &manager.Manager{}
	mgr.SetExternalMCPs(connections)
	mgr.SetBigPictures(pictures)
	router := newRouter(ctx, &config.Config{Server: config.Server{BasePath: "/krabby"}, MCP: config.MCP{Path: "/mcp"}}, routeServices{bigPictures: mgr, tracing: mgr}, nil, nil, nil)
	request := func(method, suffix string, payload any, want int, output any) {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, "/krabby/api/v1/big-pictures"+suffix, strings.NewReader(string(data))))
		if rec.Code != want {
			t.Fatalf("%s %s = %d want %d: %s", method, suffix, rec.Code, want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret-token") {
			t.Fatal("source selection exposed credentials")
		}
		if output != nil {
			if err := json.Unmarshal(rec.Body.Bytes(), output); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := bigpicture.Config{Name: "commerce", Title: "Commerce", Namespace: "commerce", Prompt: "Explain deployment", Sources: []bigpicture.Source{{Kind: "mcp", Ref: "config"}}}
	var picture bigpicture.Picture
	request("POST", "", cfg, 201, &picture)
	request("POST", "", cfg, 409, nil)
	request("GET", "/source-options?kind=mcp", nil, 200, nil)
	var page bigpicture.Page
	request("GET", "?namespace=commerce", nil, 200, &page)
	if len(page.Items) != 1 {
		t.Fatal("namespace listing failed")
	}
	request("GET", "/commerce/snapshot", nil, 404, nil)
	request("POST", "/commerce/generate", nil, 503, nil)
	pub := bigpicture.Publication{ExpectedVersion: 1, Producer: "test-agent", Overview: "overview.md", Documents: []bigpicture.Document{{Path: "overview.md", Title: "Overview", Markdown: "# Architecture\nSee [services](services/payment.md)."}, {Path: "services/payment.md", Title: "Payment", Markdown: "Payment details"}}}
	var manifest bigpicture.Snapshot
	request("POST", "/commerce/publish", pub, 201, &manifest)
	request("POST", "/commerce/publish", pub, 409, nil)
	var doc bigpicture.DocumentRead
	request("GET", "/commerce/document?path=services/payment.md&revision="+manifest.ID, nil, 200, &doc)
	if doc.Content != "Payment details" || doc.Revision != manifest.ID {
		t.Fatal("document read failed")
	}
	request("GET", "/commerce/document?path=../escape.md", nil, 404, nil)
	request("GET", "/commerce/document?path=overview.md&offset=-1", nil, 400, nil)
	request("PUT", "/commerce", cfg, 409, nil)
	cfg.ExpectedVersion, cfg.Prompt = 1, "Explain another scope"
	request("PUT", "/commerce", cfg, 200, &picture)
	request("GET", "/commerce/snapshot?revision="+manifest.ID, nil, 200, nil)
	request("GET", "?namespace=commerce", nil, 200, &page)
	if !page.Items[0].Stale {
		t.Fatal("outdated publication not marked")
	}
	request("DELETE", "/commerce?expected_version=1", nil, 409, nil)
	request("DELETE", "/commerce?expected_version=2", nil, 200, nil)
	request("GET", "/commerce", nil, 404, nil)
}
