package server

import (
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/rakunlabs/ada"
	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/transfer"
)

// MCP uses POST for reads and DELETE for transport sessions, so a blanket HTTP
// method ban would break queries. Its core/API catalogs separately omit writes.
func readOnlyMiddleware(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.ReadOnly {
				p := path.Clean(strings.TrimPrefix(path.Clean(r.URL.Path), cfg.Server.BasePath))
				admin := cfg.MCP.Path + "/admin"
				blocked := p == admin || strings.HasPrefix(p, admin+"/") || p == "/webhook/git"
				transport := p == cfg.MCP.Path || p == cfg.MCP.Path+"/api"
				if !transport && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
					blocked = true
				}
				if blocked {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":"Krabby is read-only"}`))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func syncStatus(cfg *config.Config) ada.HandlerFunc {
	return func(c *ada.Context) error {
		status, err := transfer.ReadStatus(filepath.Join(cfg.DataDir, "manifest.json"))
		if errors.Is(err, os.ErrNotExist) {
			return c.SetStatus(http.StatusNotFound).Err(errors.New("no imported dataset"))
		}
		if err != nil {
			return c.Err(err)
		}
		return c.SendJSON(status)
	}
}
