// Package server wires the ada HTTP server: REST API, git webhook and the
// MCP endpoint.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/pprof"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rakunlabs/ada"
	mcors "github.com/rakunlabs/ada/middleware/cors"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"
	mserver "github.com/rakunlabs/ada/middleware/server"
	mtelemetry "github.com/rakunlabs/ada/middleware/telemetry"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/manager"
)

// Handler builds the configured HTTP route graph without binding a socket.
func Handler(ctx context.Context, cfg *config.Config, mgr *manager.Manager, mcpServer, mcpAPIServer, mcpAdminServer *mcp.Server) http.Handler {
	return newRouter(ctx, cfg, managerRouteServices(mgr), mcpServer, mcpAPIServer, mcpAdminServer)
}

// Start runs the HTTP server until ctx is cancelled.
func Start(ctx context.Context, cfg *config.Config, mgr *manager.Manager, mcpServer, mcpAPIServer, mcpAdminServer *mcp.Server) error {
	server := newRouter(ctx, cfg, managerRouteServices(mgr), mcpServer, mcpAPIServer, mcpAdminServer)

	return server.StartWithContext(ctx, cfg.Server.Host+":"+cfg.Server.Port)
}

func newRouter(ctx context.Context, cfg *config.Config, services routeServices, mcpServer, mcpAPIServer, mcpAdminServer *mcp.Server) *ada.Server {
	server := ada.New()
	server.Use(
		mrecover.Middleware(),
		mserver.Middleware(config.ServiceName+":"+config.Version),
		mcors.Middleware(),
		mrequestid.Middleware(),
		mlog.Middleware(),
		mtelemetry.Middleware(),
	)

	// base mounts every route (UI, REST, MCP, webhook, healthz) under the
	// configured base path, e.g. "/krabby". An empty base path serves at root.
	basePath := cfg.Server.BasePath
	base := server.Group(basePath)

	base.GET("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	if cfg.Server.Pprof {
		mountPprof(base)
	}

	// Each path owns a disjoint tool catalog. Clients add only the capabilities
	// they need and can enable or disable API and administration independently.
	for path, catalog := range mcpCatalogRoutes(cfg.MCP.Path, mcpServer, mcpAPIServer, mcpAdminServer) {
		handler := mcp.NewStreamableHTTPHandler(
			func(_ *http.Request) *mcp.Server { return catalog },
			&mcp.StreamableHTTPOptions{},
		)
		// The MCP paths carry no authentication of their own: krabby is deployed
		// behind a proxy that authenticates every request before it arrives.
		base.Handle(path, mcpProbe(handler))
	}

	// The Langfuse HTTP scope is attached to the API group rather than the
	// whole mux on purpose: /healthz, the pprof endpoints, the embedded UI
	// assets and the MCP path itself either carry no LLM work at all or are
	// already traced by their own scope, and Langfuse bills per observation.
	// A liveness probe every ten seconds is 8.6k observations a day of nothing.
	api := base.Group("/api/v1", langfuseMiddleware(services.tracing))
	api.GET("/settings", server.Wrap(getSettings(cfg, services.system)))
	api.GET("/browser-extension.zip", server.Wrap(downloadBrowserExtension()))
	api.GET("/repos", server.Wrap(listRepos(services.repos)))
	api.GET("/repos/owners", server.Wrap(listRepoOwners(services.repos)))
	api.GET("/repos/namespaces", server.Wrap(listRepoNamespaces(services.repos)))
	api.GET("/repos/active", server.Wrap(listActiveRepos(services.repos)))
	api.GET("/tasks", server.Wrap(listTasks(services.queue)))
	api.DELETE("/tasks/history", server.Wrap(clearTaskHistory(services.queue)))
	api.DELETE("/tasks/queued", server.Wrap(cancelPendingTasks(services.queue)))
	api.PUT("/tasks/concurrency", server.Wrap(setTaskConcurrency(services.queue)))
	api.POST("/tasks/{seq}/-/bump", server.Wrap(bumpTask(services.queue)))
	api.DELETE("/tasks/{seq}", server.Wrap(cancelTask(services.queue)))
	api.POST("/repos", server.Wrap(addRepo(services.repos)))

	// Namespace metadata (name + description). The repo tags themselves live on
	// the repo records; these routes only manage the human/LLM-facing
	// descriptions. GET reuses /repos/namespaces above (count + description).
	api.GET("/namespaces", server.Wrap(listRepoNamespaces(services.repos)))
	api.POST("/namespaces", server.Wrap(upsertNamespace(services.repos)))
	api.DELETE("/namespaces/{name}", server.Wrap(deleteNamespace(services.repos)))

	// Repo ids are full paths (host/group/.../name) with any number of "/"
	// segments, so repo-scoped routes use a greedy wildcard and a GitLab-style
	// "/-/" separator between the id and the action:
	//   GET    /repos/<id>                  repo record
	//   POST   /repos/<id>/-/refresh        queue refresh
	//   GET    /repos/<id>/-/files          list clone files
	//   ...
	api.GET("/repos/{ref...}", server.Wrap(dispatchRepo(services.repos, map[string]ada.HandlerFunc{
		"":       getRepo(services.repos),
		"graph":  repoArtifact(services.repos, graphbuilder.GraphPath),
		"report": repoArtifact(services.repos, graphbuilder.ReportPath),
		"html":   repoArtifact(services.repos, graphbuilder.HTMLPath),
		"files":  listRepoFiles(services.docs),
		"glob":   globRepoFiles(services.docs),
		"file":   readRepoFile(services.docs),
		"docs":   listDocs(services.docs),
		"doc":    getDoc(services.docs),
		// The effective build configuration of this repo: the install-wide
		// settings, this repo's overrides, and the merge the next build runs.
		"settings": repoSettings(services.repos),
	})))
	api.POST("/repos/{ref...}", server.Wrap(dispatchRepo(services.repos, map[string]ada.HandlerFunc{
		"refresh":   refreshRepo(services.repos),
		"generate":  generateRepo(services.repos),
		"cancel":    cancelRepoJob(services.repos),
		"namespace": setRepoNamespace(services.repos),
		"overrides": setRepoOverrides(services.repos),
	})))
	api.DELETE("/repos/{ref...}", server.Wrap(dispatchRepo(services.repos, map[string]ada.HandlerFunc{
		"": deleteRepo(services.repos),
	})))
	// Web content sources (wikis, Confluence spaces): named collections whose
	// pages are synced to markdown and indexed into the docs RAG.
	api.GET("/sources", server.Wrap(listSources(services.sources)))
	api.POST("/sources", server.Wrap(addSource(services.sources)))
	api.POST("/sources/config/test", server.Wrap(testSourceConfig(services.sources)))
	api.GET("/sources/{name}", server.Wrap(getSource(services.sources)))
	api.PUT("/sources/{name}", server.Wrap(updateSource(services.sources)))
	api.DELETE("/sources/{name}", server.Wrap(deleteSource(services.sources)))
	api.POST("/sources/{name}/refresh", server.Wrap(refreshSource(services.sources)))
	api.POST("/sources/{name}/cancel", server.Wrap(cancelSource(services.sources)))
	api.POST("/sources/{name}/pages", server.Wrap(addSourcePage(services.sources)))
	api.POST("/sources/{name}/pages/import", server.Wrap(importSourcePages(services.sources)))
	api.POST("/sources/{name}/sitemap", server.Wrap(importSourceSitemap(services.sources)))
	api.DELETE("/sources/{name}/pages", server.Wrap(deleteSourcePage(services.sources)))
	api.GET("/sources/{name}/doc", server.Wrap(getSourceDoc(services.sources)))

	// API catalog: OpenAPI documents and gRPC servers, catalogued as groups of
	// services whose endpoints are browsable and indexed into the docs RAG.
	api.GET("/apis/groups", server.Wrap(listAPIGroups(services.apis)))
	api.POST("/apis/groups", server.Wrap(upsertAPIGroup(services.apis)))
	api.DELETE("/apis/groups/{name}", server.Wrap(deleteAPIGroup(services.apis)))
	api.GET("/apis/kinds", server.Wrap(listAPIServiceKinds(services.apis)))
	api.GET("/apis/services", server.Wrap(listAPIServices(services.apis)))
	api.POST("/apis/services", server.Wrap(addAPIService(services.apis)))
	api.POST("/apis/services/config/test", server.Wrap(testAPIServiceConfig(services.apis)))
	api.GET("/apis/services/{name}", server.Wrap(getAPIService(services.apis)))
	api.PUT("/apis/services/{name}", server.Wrap(updateAPIService(services.apis)))
	api.DELETE("/apis/services/{name}", server.Wrap(deleteAPIService(services.apis)))
	api.POST("/apis/services/{name}/refresh", server.Wrap(refreshAPIService(services.apis)))
	api.POST("/apis/services/{name}/cancel", server.Wrap(cancelAPIService(services.apis)))
	api.GET("/apis/services/{name}/operation", server.Wrap(getAPIOperation(services.apis)))
	api.POST("/apis/services/{name}/operation/call", server.Wrap(callAPIOperation(services.apis)))

	api.GET("/docs/search", server.Wrap(searchDocs(services.docs)))
	api.GET("/code/search", server.Wrap(searchCode(services.docs)))
	api.GET("/docs/config", server.Wrap(getDocsConfig(services.docsConfig)))
	api.PUT("/docs/config", server.Wrap(setDocsConfig(services.docsConfig)))
	api.POST("/docs/config/test/llm", server.Wrap(testLLM(services.docsConfig)))
	api.POST("/docs/config/test/embedder", server.Wrap(testEmbedder(services.docsConfig)))
	api.POST("/docs/config/test/code-embedder", server.Wrap(testCodeEmbedder(services.docsConfig)))
	api.POST("/docs/config/test/langfuse", server.Wrap(testLangfuse(services.docsConfig)))
	api.GET("/graph", mergedGraph(services.docs))
	api.GET("/credentials", server.Wrap(listCredentials(services.credentials)))
	api.PUT("/credentials", server.Wrap(setCredential(services.credentials)))
	api.DELETE("/credentials", server.Wrap(deleteCredential(services.credentials)))
	api.GET("/external-mcps", server.Wrap(listExternalMCPs(services.externalMCPs)))
	api.POST("/external-mcps", server.Wrap(saveExternalMCP(services.externalMCPs, true)))
	api.PUT("/external-mcps/{name}", server.Wrap(saveExternalMCP(services.externalMCPs, false)))
	api.DELETE("/external-mcps/{name}", server.Wrap(deleteExternalMCP(services.externalMCPs)))
	api.POST("/external-mcps/discover", server.Wrap(discoverExternalMCP(services.externalMCPs)))
	api.GET("/big-pictures", server.Wrap(listBigPictures(services.bigPictures)))
	api.POST("/big-pictures", server.Wrap(saveBigPicture(services.bigPictures, true)))
	api.GET("/big-pictures/source-options", server.Wrap(bigPictureSourceOptions(services.bigPictures)))
	api.GET("/big-pictures/{name}", server.Wrap(getBigPicture(services.bigPictures)))
	api.PUT("/big-pictures/{name}", server.Wrap(saveBigPicture(services.bigPictures, false)))
	api.DELETE("/big-pictures/{name}", server.Wrap(deleteBigPicture(services.bigPictures)))
	api.POST("/big-pictures/{name}/publish", server.Wrap(publishBigPicture(services.bigPictures)))
	api.POST("/big-pictures/{name}/generate", server.Wrap(generateBigPicture(services.bigPictures)))
	api.GET("/big-pictures/{name}/snapshot", server.Wrap(bigPictureSnapshot(services.bigPictures)))
	api.GET("/big-pictures/{name}/document", server.Wrap(readBigPictureDocument(services.bigPictures)))

	base.POST("/webhook/git", gitWebhook(services.webhook))

	// Web UI: embedded Svelte SPA served at the base path with client-side
	// routing fallback. Concrete routes above (/api, /mcp, /webhook, /healthz)
	// take precedence over this catch-all wildcard. The handler is told the
	// base path so it can strip the prefix before serving assets and inject it
	// into index.html for the client.
	uiHandler, built := webHandler(basePath)
	if !built {
		slog.Warn("web UI not built; serving placeholder (run `make build-ui`)")
	}

	// When served under a base path, redirect the bare prefix (e.g. "/krabby")
	// to "/krabby/" so relative asset URLs resolve correctly.
	if basePath != "" {
		server.GET(basePath, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, basePath+"/", http.StatusMovedPermanently)
		})
	}

	base.HandleWildcard("/", uiHandler)

	return server
}

func mcpCatalogRoutes(root string, core, api, admin *mcp.Server) map[string]*mcp.Server {
	return map[string]*mcp.Server{
		root:            core,
		root + "/api":   api,
		root + "/admin": admin,
	}
}

// mountPprof registers the runtime profiling endpoints (goroutine, heap, CPU,
// ...) under <base>/debug/pprof/, for diagnosing stuck or slow work such as a
// hung vector upsert. Fetch e.g. /debug/pprof/goroutine?debug=2.
//
// Only called when server.pprof is enabled. The handlers are unauthenticated
// like every other route, and heap dumps carry process memory, so mounting them
// is a decision the operator makes rather than a default.
func mountPprof(base *ada.Mux) {
	base.GET("/debug/pprof/", http.HandlerFunc(pprof.Index))
	base.GET("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	base.GET("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	base.GET("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	base.GET("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
	// Named profiles (goroutine, heap, allocs, ...) are served by their own
	// handler rather than pprof.Index, because Index only recognises a named
	// profile when the URL path is exactly "/debug/pprof/<name>". Under a base
	// path (e.g. "/krabby/debug/pprof/goroutine") that check fails and Index
	// would always return the profile list. Resolving the handler by the {name}
	// segment keeps ?debug=2 (full text dump) working behind the base path.
	base.GET("/debug/pprof/{name}", func(w http.ResponseWriter, r *http.Request) {
		pprof.Handler(r.PathValue("name")).ServeHTTP(w, r)
	})
}
