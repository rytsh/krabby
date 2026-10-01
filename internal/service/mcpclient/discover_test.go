package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDiscoveryRealSession(t *testing.T) {
	remote := mcp.NewServer(&mcp.Implementation{Name: "external-config", Version: "test"}, &mcp.ServerOptions{PageSize: 1})
	var executed atomic.Int32
	for _, name := range []string{"get_config", "delete_config"} {
		mcp.AddTool(remote, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			executed.Add(1)
			return nil, nil, nil
		})
	}
	remote.AddResource(&mcp.Resource{Name: "deployment", URI: "config://production/deployment"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		executed.Add(1)
		return nil, nil
	})
	remote.AddResourceTemplate(&mcp.ResourceTemplate{Name: "environment", URITemplate: "config://{env}/services"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		executed.Add(1)
		return nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer token-secret" || r.Header.Get("X-Key") != "header-secret" {
			t.Error("credentials not supplied")
		}
		if r.Method == http.MethodGet {
			t.Error("unexpected standalone SSE")
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	c, err := merge(nil, Config{Name: "test", URL: server.URL, BearerToken: "token-secret", Headers: map[string]string{"X-Key": "header-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	result := Discover(context.Background(), c)
	if !result.OK {
		t.Fatalf("discovery: %+v", result)
	}
	if len(result.Tools) != 2 || len(result.Resources) != 1 || len(result.ResourceTemplates) != 1 || result.Server.Name != "external-config" || result.Truncated {
		t.Fatalf("catalog: %+v", result)
	}
	if executed.Load() != 0 {
		t.Fatal("discovery executed a tool or read resource content")
	}
	if requests.Load() < 4 {
		t.Fatal("discovery did not list catalogs")
	}
}

func TestDiscoveryToolsOnly(t *testing.T) {
	remote := mcp.NewServer(&mcp.Implementation{Name: "tools-only", Version: "test"}, nil)
	mcp.AddTool(remote, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, nil, nil
	})
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	defer server.Close()
	c, _ := merge(nil, Config{Name: "test", URL: server.URL})
	result := Discover(context.Background(), c)
	if !result.OK || len(result.Tools) != 1 || len(result.Resources) != 0 {
		t.Fatalf("tools-only discovery: %+v", result)
	}
}

func TestDiscoveryRedirectsAndErrorsDoNotLeakSecrets(t *testing.T) {
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationRequests.Add(1) }))
	defer destination.Close()
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
					return
				}
				http.Error(w, r.Header.Get("Authorization")+r.Header.Get("X-Key"), http.StatusUnauthorized)
			}))
			defer server.Close()
			c, _ := merge(nil, Config{Name: "test", URL: server.URL, BearerToken: "token-secret", Headers: map[string]string{"X-Key": "header-secret"}})
			result := Discover(context.Background(), c)
			if result.OK || result.Error == "" {
				t.Fatalf("failure not reported: %+v", result)
			}
			data, _ := json.Marshal(result)
			if strings.Contains(string(data), "token-secret") || strings.Contains(string(data), "header-secret") {
				t.Fatal("remote error exposed secrets")
			}
		})
	}
	if destinationRequests.Load() != 0 {
		t.Fatal("followed redirect with credentials")
	}
}

func TestDiscoveryTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	c, _ := merge(nil, Config{Name: "test", URL: server.URL, TimeoutSeconds: 1})
	start := time.Now()
	result := Discover(context.Background(), c)
	if result.OK || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout failed: %+v", result)
	}
}

func TestCatalogBounds(t *testing.T) {
	t.Run("pagination", func(t *testing.T) {
		calls := 0
		items, truncated, err := listCatalog(context.Background(), func(cursor string) ([]int, string, error) {
			calls++
			if cursor == "" {
				return []int{1}, "next", nil
			}
			return []int{2}, "", nil
		})
		if err != nil || truncated || len(items) != 2 || calls != 2 {
			t.Fatal("pagination failed")
		}
	})
	t.Run("item cap", func(t *testing.T) {
		items, truncated, err := listCatalog(context.Background(), func(string) ([]int, string, error) { return make([]int, 501), "", nil })
		if err != nil || !truncated || len(items) != 500 {
			t.Fatal("item cap failed")
		}
	})
	t.Run("repeated cursor", func(t *testing.T) {
		calls := 0
		_, truncated, err := listCatalog(context.Background(), func(string) ([]int, string, error) { calls++; return nil, "same", nil })
		if err != nil || !truncated || calls != 2 {
			t.Fatal("cursor cycle not bounded")
		}
	})
	t.Run("empty pages", func(t *testing.T) {
		calls := 0
		_, truncated, err := listCatalog(context.Background(), func(string) ([]int, string, error) { calls++; return nil, fmt.Sprint(calls), nil })
		if err != nil || !truncated || calls != 50 {
			t.Fatal("page cap failed")
		}
	})
}

func TestResponseBudgets(t *testing.T) {
	var budget atomic.Int64
	budget.Store(8)
	first := &limitedBody{ReadCloser: io.NopCloser(strings.NewReader("12345")), remaining: 4, total: &budget}
	data, err := io.ReadAll(first)
	if err == nil || string(data) != "1234" {
		t.Fatal("per-response limit not enforced")
	}
	second := &limitedBody{ReadCloser: io.NopCloser(strings.NewReader("56789")), remaining: 10, total: &budget}
	data, err = io.ReadAll(second)
	if err == nil || string(data) != "5678" {
		t.Fatal("shared discovery budget not enforced")
	}
}
