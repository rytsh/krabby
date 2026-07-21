// Package web embeds the built Svelte SPA (source in _ui, built into web/dist)
// and serves it with a client-side routing fallback.
package web

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"strings"
)

// dist embeds the built SPA. Build it with `make ui` (or
// `cd _ui && pnpm install && pnpm build`) before compiling krabby.
//
// The all: prefix embeds files whose names start with _ or . too (Vite may emit
// such asset names).
//
//go:embed all:dist
var dist embed.FS

// Handler returns an http.Handler that serves the embedded SPA. Static assets
// are served directly; any other path falls back to index.html so the client
// router can handle it. If the UI was never built, it returns a placeholder and
// ok=false so the caller can decide how to mount it.
func Handler() (h http.Handler, ok bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return placeholder(), false
	}

	if _, err := fs.Stat(sub, "index.html"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return placeholder(), false
		}

		return placeholder(), false
	}

	fileServer := http.FileServerFS(sub)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath := strings.TrimPrefix(r.URL.Path, "/")
		if reqPath == "" {
			reqPath = "index.html"
		}

		if _, err := fs.Stat(sub, reqPath); err != nil {
			// Unknown asset: serve the SPA entrypoint for client-side routing.
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}

		fileServer.ServeHTTP(w, r)
	}), true
}

func placeholder() http.Handler {
	const page = `<!doctype html><html><head><meta charset="utf-8">` +
		`<title>krabby</title></head>` +
		`<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem">` +
		`<h1>krabby</h1><p>The web UI has not been built yet.</p>` +
		`<p>Build it with <code>make ui</code> ` +
		`(or <code>cd _ui &amp;&amp; pnpm install &amp;&amp; pnpm build</code>), then rebuild krabby.</p>` +
		`<p>The REST API is at <code>/api/v1</code> and the MCP endpoint at <code>/mcp</code>.</p>` +
		`</body></html>`

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
}
