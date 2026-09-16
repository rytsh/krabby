package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rytsh/krabby/internal/config"
)

func TestReadOnlyHTTPBoundary(t *testing.T) {
	for _, base := range []string{"", "/krabby"} {
		cfg := &config.Config{ReadOnly: true, Server: config.Server{BasePath: base}, MCP: config.MCP{Path: "/tools"}}
		for _, tt := range []struct {
			method, path string
			allowed      bool
		}{
			{"GET", "/api/v1/repos", true},
			{"GET", "/api/v1/docs/search", true},
			{"GET", "/api/v1/sync/status", true},
			{"POST", "/api/v1/repos", false},
			{"PUT", "/api/v1/docs/config", false},
			{"DELETE", "/api/v1/sources/wiki/pages", false},
			{"POST", "/api/v1/apis/services/a/operation/call", false},
			{"POST", "/webhook/git", false},
			{"POST", "/tools/admin", false},
			{"GET", "/tools/admin/", false},
			{"POST", "/tools", true},
			{"POST", "/tools/api", true},
			{"DELETE", "/tools", true},
			{"POST", "//api/v1/repos", false},
			{"POST", "/tools/../api/v1/repos", false},
		} {
			t.Run(base+"/"+tt.method+tt.path, func(t *testing.T) {
				called := false
				h := readOnlyMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(tt.method, base+tt.path, nil))
				if called != tt.allowed {
					t.Fatalf("called=%v status=%d", called, w.Code)
				}
				if !tt.allowed && w.Code != http.StatusForbidden {
					t.Fatalf("status=%d", w.Code)
				}
			})
		}
	}
}
