package server

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/rakunlabs/ada/handler/folder"
)

// dist embeds the built Svelte SPA (source in _ui, built into
// internal/server/dist). Build it with `make build-ui` (or `cd _ui && pnpm install &&
// pnpm build`) before compiling krabby; a .gitkeep placeholder keeps the embed
// pattern valid when the UI has not been built.
//
// The all: prefix embeds files whose names start with _ or . too (Vite may emit
// such asset names).
//
//go:embed all:dist
var dist embed.FS

// webHandler returns an http.Handler that serves the embedded SPA under the
// given base path using ada's folder handler in SPA mode. The UI is built with
// relative asset URLs (Vite base "./") and a hash router, so it is base-path
// agnostic; the handler's PrefixPath strips the base prefix before looking
// assets up in the embedded FS, and SPA mode falls back to index.html for any
// unknown route. If the UI was never built, it returns a placeholder and
// ok=false so the caller can decide how to mount it.
func webHandler(basePath string) (h http.Handler, ok bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return webPlaceholder(), false
	}

	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return webPlaceholder(), false
	}

	f, err := folder.New(&folder.Config{
		PrefixPath:     basePath,
		SPA:            true,
		Index:          true,
		StripIndexName: true,
		CacheRegex: []*folder.RegexCacheStore{
			{Regex: `index\.html$`, CacheControl: "no-cache"},
			{Regex: `.*\.(js|css|wasm|svg|woff2?)$`, CacheControl: "public, max-age=604800, immutable"},
		},
	})
	if err != nil {
		return webPlaceholder(), false
	}

	f.SetFs(http.FS(sub))

	return f, true
}

func webPlaceholder() http.Handler {
	const page = `<!doctype html><html><head><meta charset="utf-8">` +
		`<title>krabby</title></head>` +
		`<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem">` +
		`<h1>krabby</h1><p>The web UI has not been built yet.</p>` +
		`<p>Build it with <code>make build-ui</code> ` +
		`(or <code>cd _ui &amp;&amp; pnpm install &amp;&amp; pnpm build</code>), then rebuild krabby.</p>` +
		`<p>The REST API is at <code>/api/v1</code> and the MCP endpoint at <code>/mcp</code>.</p>` +
		`</body></html>`

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
}
