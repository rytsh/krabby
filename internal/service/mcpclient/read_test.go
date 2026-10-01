package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rytsh/krabby/internal/storage"
)

func TestReadResourceRequiresCurrentExactGrant(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	var reads, requests atomic.Int32
	remote := mcp.NewServer(&mcp.Implementation{Name: "config", Version: "test"}, nil)
	remote.AddResource(&mcp.Resource{Name: "deployment", URI: "config://prod"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		reads.Add(1)
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "config://prod", Text: "deployment: payment"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing authentication")
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg := Config{Name: "config", URL: server.URL, BearerToken: "test-secret", AllowedResources: []string{"config://prod", "config://{env}"}}
	if _, err := s.Save(ctx, "", cfg); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"config://ungranted", "config://{env}"} {
		if _, _, err := s.ReadResource(ctx, "config", uri, 10); err == nil {
			t.Fatal("ungranted/template resource read")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("denied resource made network requests")
	}
	text, truncated, err := s.ReadResource(ctx, "config", "config://prod", 10)
	if err != nil || !truncated || text != "deployment" {
		t.Fatalf("resource: %q %t %v", text, truncated, err)
	}
	cfg.BearerToken, cfg.AllowedResources = "", nil
	if _, err := s.Save(ctx, "config", cfg); err != nil {
		t.Fatal(err)
	}
	count := requests.Load()
	if _, _, err := s.ReadResource(ctx, "config", "config://prod", 10); err == nil {
		t.Fatal("revoked grant read")
	}
	if requests.Load() != count || reads.Load() != 1 {
		t.Fatal("revocation did not prevent read")
	}
}

func TestReadResourceDoesNotEchoRemoteErrors(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "credential-secret", 401) }))
	defer server.Close()
	if _, err := s.Save(context.Background(), "", Config{Name: "config", URL: server.URL, AllowedResources: []string{"config://prod"}}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.ReadResource(context.Background(), "config", "config://prod", 10)
	if err == nil || strings.Contains(err.Error(), "credential-secret") {
		t.Fatal("remote error was exposed")
	}
}
