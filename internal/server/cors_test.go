package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/queue"
)

type countingQueue struct{ cleared int }

func (*countingQueue) TaskSnapshot() queue.Snapshot { return queue.Snapshot{} }
func (q *countingQueue) ClearTaskHistory()          { q.cleared++ }
func (*countingQueue) CancelPendingTasks() int      { return 0 }
func (*countingQueue) SetTaskConcurrency(int)       {}
func (*countingQueue) BumpTask(uint64) bool         { return false }
func (*countingQueue) CancelTask(uint64) bool       { return false }

func corsRouter(t *testing.T, origins ...string) (http.Handler, *countingQueue) {
	t.Helper()

	q := &countingQueue{}
	cfg := &config.Config{
		Server: config.Server{AllowOrigins: origins},
		MCP:    config.MCP{Path: "/mcp"},
	}

	return newRouter(context.Background(), cfg, routeServices{queue: q, tracing: &manager.Manager{}}, MCPCatalogs{}), q
}

func serve(h http.Handler, method, path string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

const (
	otherSite   = "https://other.example"
	trustedSite = "https://tools.example.com"
	clearPath   = "/api/v1/tasks/history"
)

// Nothing authenticates a caller, so by default no other site's page may read
// an answer or get a preflight approved.
func TestCORSIsClosedByDefault(t *testing.T) {
	h, _ := corsRouter(t)

	rec := serve(h, http.MethodGet, "/healthz", map[string]string{"Origin": otherSite})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("GET answered Access-Control-Allow-Origin %q, want none", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("GET status = %d, want 200: withholding CORS must not break the request itself", rec.Code)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := serve(h, http.MethodOptions, clearPath, map[string]string{
			"Origin":                         otherSite,
			"Access-Control-Request-Method":  method,
			"Access-Control-Request-Headers": "content-type",
		})
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("preflight for %s approved origin %q, want none", method, got)
		}
	}
}

// A request that needs no preflight reaches the server whatever the CORS
// headers say, so the mutation itself has to be refused.
func TestCrossSiteMutationIsRefusedByDefault(t *testing.T) {
	tests := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{
			name:   "another site, modern browser",
			header: map[string]string{"Origin": otherSite, "Sec-Fetch-Site": "cross-site"},
			want:   http.StatusForbidden,
		},
		{
			name:   "another site, browser without fetch metadata",
			header: map[string]string{"Origin": otherSite},
			want:   http.StatusForbidden,
		},
		{
			name:   "the web UI",
			header: map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"},
			want:   http.StatusOK,
		},
		{
			name:   "the web UI over plain http, which sends no fetch metadata",
			header: map[string]string{"Origin": "http://example.com"},
			want:   http.StatusOK,
		},
		{
			name: "a client that is not a browser",
			want: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, q := corsRouter(t)

			rec := serve(h, http.MethodDelete, clearPath, tt.header)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}

			wantCleared := 0
			if tt.want == http.StatusOK {
				wantCleared = 1
			}
			if q.cleared != wantCleared {
				t.Errorf("history cleared %d times, want %d", q.cleared, wantCleared)
			}
			if tt.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), "server.allow_origins") {
				t.Errorf("refusal %q does not name the setting that fixes it", rec.Body.String())
			}
		})
	}
}

func TestConfiguredOriginIsAllowed(t *testing.T) {
	tests := []struct {
		name    string
		origins []string
		origin  string
		allowed bool
	}{
		{name: "exact entry", origins: []string{trustedSite}, origin: trustedSite, allowed: true},
		{name: "unlisted origin", origins: []string{trustedSite}, origin: otherSite},
		{name: "wildcard entry", origins: []string{"https://*.example.com"}, origin: trustedSite, allowed: true},
		{name: "wildcard does not cover another domain", origins: []string{"https://*.example.com"}, origin: otherSite},
		{name: "star allows everything", origins: []string{"*"}, origin: otherSite, allowed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, q := corsRouter(t, tt.origins...)

			rec := serve(h, http.MethodGet, "/healthz", map[string]string{"Origin": tt.origin})
			if got := rec.Header().Get("Access-Control-Allow-Origin") != ""; got != tt.allowed {
				t.Errorf("read allowed = %v, want %v", got, tt.allowed)
			}

			rec = serve(h, http.MethodDelete, clearPath, map[string]string{
				"Origin": tt.origin, "Sec-Fetch-Site": "cross-site",
			})
			if got := rec.Code == http.StatusOK; got != tt.allowed {
				t.Errorf("mutation status = %d, allowed want %v", rec.Code, tt.allowed)
			}
			if got := q.cleared == 1; got != tt.allowed {
				t.Errorf("history cleared %d times, allowed want %v", q.cleared, tt.allowed)
			}
		})
	}
}

// Behind a proxy that rewrites Host, the UI's own requests carry an Origin that
// no longer matches it. The host the browser asked for is in X-Forwarded-Host.
func TestWebUIBehindHostRewritingProxy(t *testing.T) {
	tests := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{
			name:   "plain http, proxy forwards the original host",
			header: map[string]string{"Origin": "http://krabby.lan", "X-Forwarded-Host": "krabby.lan"},
			want:   http.StatusOK,
		},
		{
			name:   "forwarded host lists several proxies",
			header: map[string]string{"Origin": "http://krabby.lan:8080", "X-Forwarded-Host": "krabby.lan:8080, inner"},
			want:   http.StatusOK,
		},
		{
			name:   "plain http, proxy forwards nothing",
			header: map[string]string{"Origin": "http://krabby.lan"},
			want:   http.StatusForbidden,
		},
		{
			name:   "another site through the same proxy",
			header: map[string]string{"Origin": otherSite, "X-Forwarded-Host": "krabby.lan"},
			want:   http.StatusForbidden,
		},
		{
			name: "browser says cross-site, forwarded host cannot overrule it",
			header: map[string]string{
				"Origin": "http://krabby.lan", "X-Forwarded-Host": "krabby.lan", "Sec-Fetch-Site": "cross-site",
			},
			want: http.StatusForbidden,
		},
		{
			name:   "https, browser vouches for the request itself",
			header: map[string]string{"Origin": "https://krabby.corp", "Sec-Fetch-Site": "same-origin"},
			want:   http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := corsRouter(t)

			req := httptest.NewRequest(http.MethodDelete, clearPath, nil)
			req.Host = "krabby-upstream:8080"
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

// A browser-based MCP client sends headers a preflight has to approve by name.
func TestAllowedOriginCanPreflightMCP(t *testing.T) {
	h, _ := corsRouter(t, trustedSite)

	rec := serve(h, http.MethodOptions, "/mcp/admin", map[string]string{
		"Origin":                         trustedSite,
		"Access-Control-Request-Method":  http.MethodPost,
		"Access-Control-Request-Headers": "content-type,mcp-session-id,mcp-protocol-version",
	})

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != trustedSite {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, trustedSite)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "*" {
		t.Errorf("Access-Control-Allow-Headers = %q, want *", got)
	}
}
