package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/storage"
)

func TestExternalMCPRoutes(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mcpclient.New(db)
	if err != nil {
		t.Fatal(err)
	}
	mgr := &manager.Manager{}
	mgr.SetExternalMCPs(store)
	router := newRouter(context.Background(), &config.Config{Server: config.Server{BasePath: "/krabby"}, MCP: config.MCP{Path: "/mcp"}}, routeServices{externalMCPs: mgr, tracing: mgr}, nil, nil, nil)
	request := func(method, path, body string, want int) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/krabby/api/v1/external-mcps"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "token-secret") || strings.Contains(rec.Body.String(), "header-secret") {
			t.Fatal("API exposed credentials")
		}
		return rec.Body.String()
	}
	request("GET", "", "", 200)
	create := `{"name":"config","url":"https://example.com/mcp","bearer_token":"token-secret","headers":{"X-Key":"header-secret"},"allowed_tools":["get_config"]}`
	request("POST", "", create, 201)
	request("POST", "", create, 409)
	list := request("GET", "", "", 200)
	var items []mcpclient.View
	if err := json.Unmarshal([]byte(list), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].BearerTokenSet || len(items[0].HeaderNames) != 1 {
		t.Fatalf("invalid list: %s", list)
	}
	request("PUT", "/config", `{"url":"https://example.com/mcp","headers":{"X-Key":""}}`, 200)
	connection, err := store.Get(context.Background(), "config")
	if err != nil {
		t.Fatal(err)
	}
	if connection.BearerToken != "token-secret" || connection.Headers["X-Key"] != "header-secret" {
		t.Fatal("API lost credentials on update")
	}
	request("PUT", "/config", `{"name":"renamed","url":"https://example.com/mcp"}`, 400)
	request("PUT", "/missing", `{"url":"https://example.com/mcp"}`, 404)
	request("POST", "", `{"name":"bad","url":"file:///etc/passwd"}`, 400)
	request("POST", "", `{"name":"bad","unknown":"token-secret"}`, 400)
	request("POST", "", create+`{}`, 400)
	request("POST", "", `{"name":"bad","description":"`+strings.Repeat("x", 65536)+`"}`, 400)
	request("POST", "/discover", `{"existing_name":"missing","url":"https://example.com"}`, 404)
	// The remote may echo auth in an error: the REST result must still be safe.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, r.Header.Get("Authorization"), 401) }))
	defer remote.Close()
	response := request("POST", "/discover", `{"name":"draft","url":"`+remote.URL+`","bearer_token":"token-secret"}`, 200)
	var discovery mcpclient.Discovery
	if err := json.Unmarshal([]byte(response), &discovery); err != nil {
		t.Fatal(err)
	}
	if discovery.OK || discovery.Error == "" {
		t.Fatal("discovery failure not surfaced")
	}
	items, err = store.List(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatal("discovery persisted a draft")
	}
	request("DELETE", "/config", "", 200)
	request("DELETE", "/config", "", 404)
}
