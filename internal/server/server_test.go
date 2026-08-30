package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/registry"
)

type failingWebhookService struct {
	err       error
	triggered bool
}

func (*failingWebhookService) WebhookSecret() string { return "" }
func (*failingWebhookService) ResolveRepo(context.Context, string) (*registry.Repo, error) {
	return &registry.Repo{ID: "github.com/acme/widgets"}, nil
}
func (s *failingWebhookService) TriggerRefresh(string, ...string) error {
	s.triggered = true

	return s.err
}

func TestRouterBasePathAndFallbackPrecedence(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{BasePath: "/krabby"},
		MCP:    config.MCP{Path: "/mcp"},
	}
	handler := newRouter(context.Background(), cfg, routeServices{}, nil, nil, nil)

	tests := []struct {
		name         string
		path         string
		wantStatus   int
		wantType     string
		wantBody     string
		wantLocation string
	}{
		{
			name: "base path contains health route", path: "/krabby/healthz",
			wantStatus: http.StatusOK, wantBody: "OK",
		},
		{
			name: "route is not exposed outside base path", path: "/healthz",
			wantStatus: http.StatusNotFound,
		},
		{
			name: "bare base redirects before fallback", path: "/krabby",
			wantStatus: http.StatusMovedPermanently, wantLocation: "/krabby/",
		},
		{
			name: "mcp route wins over the spa fallback", path: "/krabby/mcp",
			wantStatus: http.StatusOK, wantType: "application/json", wantBody: `"transport":"mcp-streamable-http"`,
		},
		{
			name: "unknown client route uses spa fallback", path: "/krabby/repos/example",
			wantStatus: http.StatusOK, wantType: "text/html",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantType != "" && !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.wantType) {
				t.Errorf("Content-Type = %q, want prefix %q", rec.Header().Get("Content-Type"), tt.wantType)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want substring %q", rec.Body.String(), tt.wantBody)
			}
			if tt.wantLocation != "" && rec.Header().Get("Location") != tt.wantLocation {
				t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), tt.wantLocation)
			}
		})
	}
}

// pprof is opt-in. /debug/pprof/heap is a dump of process memory — git tokens,
// LLM API keys, repository content — and krabby authenticates nothing itself,
// so a profiler mounted by default is one proxy rule away from disclosing all
// of it.
func TestPprofIsOffByDefault(t *testing.T) {
	cfg := &config.Config{Server: config.Server{}, MCP: config.MCP{Path: "/mcp"}}
	handler := newRouter(context.Background(), cfg, routeServices{}, nil, nil, nil)

	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/profile",
		"/debug/pprof/symbol",
		"/debug/pprof/trace",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/heap",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		// The SPA fallback answers unknown paths, so the assertion is about the
		// content rather than the status: no profile may come back.
		if body := rec.Body.String(); strings.Contains(body, "goroutine profile") ||
			strings.Contains(body, "# runtime.MemStats") ||
			strings.Contains(body, "Types of profiles available") {
			t.Errorf("%s served a pprof profile while server.pprof is disabled", path)
		}
	}
}

func TestPprofMountsWhenEnabled(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{BasePath: "/krabby", Pprof: true},
		MCP:    config.MCP{Path: "/mcp"},
	}
	handler := newRouter(context.Background(), cfg, routeServices{}, nil, nil, nil)

	// A named profile behind a base path is the case pprof.Index cannot serve
	// itself, so it is the one worth asserting.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/krabby/debug/pprof/goroutine?debug=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "goroutine profile") {
		t.Errorf("body = %q, want a goroutine profile", rec.Body.String())
	}

	// The index must also be reachable, under the base path.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/krabby/debug/pprof/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", rec.Code)
	}
}

func TestMCPCatalogRoutes(t *testing.T) {
	core := mcp.NewServer(&mcp.Implementation{Name: "core", Version: "test"}, nil)
	api := mcp.NewServer(&mcp.Implementation{Name: "api", Version: "test"}, nil)
	admin := mcp.NewServer(&mcp.Implementation{Name: "admin", Version: "test"}, nil)

	routes := mcpCatalogRoutes("/custom/mcp", core, api, admin)
	for path, want := range map[string]*mcp.Server{
		"/custom/mcp":       core,
		"/custom/mcp/api":   api,
		"/custom/mcp/admin": admin,
	} {
		if got := routes[path]; got != want {
			t.Errorf("route %q = %p, want %p", path, got, want)
		}
	}
	if len(routes) != 3 {
		t.Fatalf("route count = %d, want 3", len(routes))
	}
}

func TestVerifyGitWebhook(t *testing.T) {
	secret := "topsecret"
	body := []byte(`{"repository":{"full_name":"rytsh/krabby"}}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	githubHeader := http.Header{"X-Hub-Signature-256": []string{valid}}
	if !verifyGitWebhook(secret, body, githubHeader) {
		t.Error("valid signature rejected")
	}

	if verifyGitWebhook(secret, body, http.Header{"X-Gitea-Signature": []string{"deadbeef"}}) {
		t.Error("invalid signature accepted")
	}

	if verifyGitWebhook(secret, body, http.Header{}) {
		t.Error("missing signature accepted")
	}

	if !verifyGitWebhook(secret, body, http.Header{"X-Gitlab-Token": []string{secret}}) {
		t.Error("valid gitlab token rejected")
	}
	if !verifyGitWebhook(secret, body, http.Header{"Authorization": []string{"Bearer " + secret}}) {
		t.Error("provider-neutral bearer token rejected")
	}
}

func TestGitEventRepoRef(t *testing.T) {
	var event gitPushEvent
	event.Project.GitHTTPURL = "https://gitlab.example.com/group/sub/project.git"
	if got := gitEventRepoRef(event); got != "gitlab.example.com/group/sub/project" {
		t.Fatalf("gitEventRepoRef() = %q", got)
	}

	event.Project.GitHTTPURL = ""
	event.Project.PathWithNamespace = "group/sub/project"
	if got := gitEventRepoRef(event); got != "group/sub/project" {
		t.Fatalf("gitEventRepoRef() fallback = %q", got)
	}
}

func TestGitWebhookDoesNotAcceptFailedRefreshEnqueue(t *testing.T) {
	wantErr := errors.New("persist queued task: disk unavailable")
	mgr := &failingWebhookService{err: wantErr}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/git", strings.NewReader(
		`{"repository":{"clone_url":"https://github.com/acme/widgets.git"}}`,
	))
	rec := httptest.NewRecorder()

	gitWebhook(mgr).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !mgr.triggered || !strings.Contains(rec.Body.String(), wantErr.Error()) {
		t.Fatalf("triggered = %v, body = %q; enqueue failure was not propagated", mgr.triggered, rec.Body.String())
	}
}


func TestValidateStages(t *testing.T) {
	tests := []struct {
		name    string
		stages  []string
		wantErr bool
	}{
		{name: "empty"},
		{name: "single", stages: []string{registry.StageDocs}},
		{
			name: "all",
			stages: []string{
				registry.StageGraph, registry.StageDocs,
				registry.StageDocsIndex, registry.StageCodeIndex,
			},
		},
		{name: "typo", stages: []string{"doc"}, wantErr: true},
		{name: "mixed", stages: []string{registry.StageDocs, "rag"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateStages(tt.stages); (err != nil) != tt.wantErr {
				t.Fatalf("validateStages(%v) error = %v, wantErr %v", tt.stages, err, tt.wantErr)
			}
		})
	}
}

// POST .../-/refresh has always been a body-less call, and the UI still sends
// it that way. Gaining an optional "skip" must not turn those into 400s.
func TestBindOptionalJSON(t *testing.T) {
	tests := []struct {
		name        string
		body        io.Reader
		contentType string
		want        []string
		wantErr     bool
	}{
		{name: "no body at all", body: nil},
		{name: "empty body", body: strings.NewReader(""), contentType: "application/json"},
		{
			name:        "skip list",
			body:        strings.NewReader(`{"skip":["docs"]}`),
			contentType: "application/json",
			want:        []string{"docs"},
		},
		{
			name:        "malformed json still fails",
			body:        strings.NewReader(`{"skip":`),
			contentType: "application/json",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/x/-/refresh", tt.body)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			var got refreshRequest
			err := bindOptionalJSON(ada.NewContext(httptest.NewRecorder(), req), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("bindOptionalJSON error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !slices.Equal(got.Skip, tt.want) {
				t.Fatalf("skip = %v, want %v", got.Skip, tt.want)
			}
		})
	}
}
