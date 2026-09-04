// Package server wires the ada HTTP server: REST API, git webhook and the
// MCP endpoint.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rakunlabs/ada"
	mcors "github.com/rakunlabs/ada/middleware/cors"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"
	mserver "github.com/rakunlabs/ada/middleware/server"
	mtelemetry "github.com/rakunlabs/ada/middleware/telemetry"
	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/settings"
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
		"graph":  repoArtifact(services.repos, graphify.GraphPath),
		"report": repoArtifact(services.repos, graphify.ReportPath),
		"html":   repoArtifact(services.repos, graphify.HTMLPath),
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

// ---- profiling --------------------------------------------------------------

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

// ---- settings handler -------------------------------------------------------

// settingsResponse is a redacted view of the running config for the UI. Secrets
// (the webhook secret) are deliberately omitted; booleans indicate
// only whether they are configured.
type settingsResponse struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	LogLevel  string `json:"log_level"`
	DataDir   string `json:"data_dir"`

	Server struct {
		Host     string `json:"host"`
		Port     string `json:"port"`
		BasePath string `json:"base_path"`
	} `json:"server"`

	MCP struct {
		Path string `json:"path"`
	} `json:"mcp"`

	Graphify struct {
		Bin          string `json:"bin"`
		Python       string `json:"python,omitempty"`
		Version      string `json:"version"`
		BuildTimeout string `json:"build_timeout"`
	} `json:"graphify"`
}

func getSettings(cfg *config.Config, system systemInfoService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var s settingsResponse

		s.Version = config.Version
		s.Commit = config.Commit
		s.BuildDate = config.Date
		s.LogLevel = cfg.LogLevel
		s.DataDir = cfg.DataDir

		s.Server.Host = cfg.Server.Host
		s.Server.Port = cfg.Server.Port
		s.Server.BasePath = cfg.Server.BasePath

		s.MCP.Path = cfg.MCP.Path

		s.Graphify.Bin = cfg.Graphify.Bin
		s.Graphify.Python = cfg.Graphify.Python
		s.Graphify.Version = system.GraphifyVersion()
		s.Graphify.BuildTimeout = cfg.Graphify.BuildTimeout.String()

		return c.SendJSON(s)
	}
}

// ---- REST handlers ----------------------------------------------------------

type addRepoRequest struct {
	URL       string             `json:"url"`
	Branch    string             `json:"branch"`
	Namespace string             `json:"namespace"`
	Overrides registry.Overrides `json:"overrides"`
	// Skip drops stages from the build this call queues. Posting an already
	// tracked url is the common "re-pull this repo" gesture, so it accepts the
	// same per-run skip list as /-/refresh. It is not persisted; the permanent
	// switch is Overrides.SkipStages.
	Skip []string `json:"skip,omitempty"`
}

// repoRef splits the greedy {ref...} path value into the raw repo id and the
// action after the "/-/" separator ("" when the ref has no action suffix).
func repoRef(r *http.Request) (id, action string) {
	ref := strings.Trim(r.PathValue("ref"), "/")
	if i := strings.Index(ref, "/-/"); i >= 0 {
		return strings.Trim(ref[:i], "/"), ref[i+3:]
	}

	return ref, ""
}

// repoID returns the canonical repo id for the request. dispatchRepo resolves
// the raw ref (which may be a legacy "owner/name" suffix) once and stores the
// canonical id as the "repo_id" path value; unresolved refs fall back to the
// raw id so handlers can produce a useful 404.
func repoID(r *http.Request) string {
	if id := r.PathValue("repo_id"); id != "" {
		return id
	}

	id, _ := repoRef(r)

	return id
}

// namespaceParam reads the optional "namespace" query param for the web UI
// search endpoints. Unlike the MCP tools (which default to the "default"
// bucket), the UI searches every namespace by default, so an absent param maps
// to NamespaceAll.
func namespaceParam(r *http.Request) string {
	if ns := r.URL.Query().Get("namespace"); ns != "" {
		return ns
	}

	return registry.NamespaceAll
}

// dispatchRepo routes /repos/{ref...} requests: it splits "<id>[/-/<action>]",
// resolves the id (exact match or unique legacy-suffix match) to its canonical
// form, and invokes the handler registered for the action.
func dispatchRepo(mgr repoReader, routes map[string]ada.HandlerFunc) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id, action := repoRef(c.Request)
		if id == "" {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "repo id is required"})
		}

		handler, ok := routes[action]
		if !ok {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": fmt.Sprintf("unknown repo action %q", action)})
		}

		repo, err := mgr.ResolveRepo(c.Request.Context(), id)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		if repo != nil {
			id = repo.ID
		}

		c.Request.SetPathValue("repo_id", id)

		return handler(c)
	}
}

// repoView decorates a repo record with the transient in-memory activity so
// the UI can show what is currently running.
type repoView struct {
	*registry.Repo
	Running string `json:"running,omitempty"`
}

func viewRepo(mgr repoReader, repo *registry.Repo) repoView {
	return repoView{Repo: repo, Running: mgr.Activity(repo.ID)}
}

// pagedRepos is the paginated envelope returned by GET /repos.
type pagedRepos struct {
	Items   []repoView `json:"items"`
	Total   int        `json:"total"`
	Page    int        `json:"page"`
	PerPage int        `json:"per_page"`
}

func listRepos(mgr repoReader) ada.HandlerFunc {
	return func(c *ada.Context) error {
		params := c.Request.URL.Query()

		// The web UI lists every namespace by default (unlike the MCP tools,
		// which default to the 'default' bucket); a namespace query param
		// narrows it. NamespaceAll ("*") is the explicit "all" value.
		namespace := params.Get("namespace")
		if namespace == "" {
			namespace = registry.NamespaceAll
		}

		opts := registry.ListOptions{
			Search:    params.Get("q"),
			Owner:     params.Get("owner"),
			Status:    params.Get("status"),
			Namespace: namespace,
		}
		if n, err := strconv.Atoi(params.Get("page")); err == nil && n > 0 {
			opts.Page = n
		}
		if n, err := strconv.Atoi(params.Get("per_page")); err == nil && n > 0 {
			opts.PerPage = n
		}

		repos, total, err := mgr.ListRepos(c.Request.Context(), opts)
		if err != nil {
			return c.Err(err)
		}

		views := make([]repoView, 0, len(repos))
		for _, repo := range repos {
			views = append(views, viewRepo(mgr, repo))
		}

		page := opts.Page
		if page <= 0 {
			page = 1
		}
		perPage := opts.PerPage
		if perPage <= 0 {
			perPage = len(views)
		}

		return c.SendJSON(pagedRepos{Items: views, Total: total, Page: page, PerPage: perPage})
	}
}

// listRepoOwners returns the owner groups (prefix + count) for the sidebar tree.
func listRepoOwners(mgr repoReader) ada.HandlerFunc {
	return func(c *ada.Context) error {
		owners, err := mgr.RepoOwners(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(owners)
	}
}

// listRepoNamespaces returns the namespace groups (namespace + count +
// description). Untagged repos are reported under "default".
func listRepoNamespaces(mgr repoReader) ada.HandlerFunc {
	return func(c *ada.Context) error {
		namespaces, err := mgr.RepoNamespaces(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(namespaces)
	}
}

type upsertNamespaceRequest struct {
	Name string `json:"name"`
	// Description is nullable: omit it to keep the stored one, send null to
	// clear it, send a value to replace it. The record is written whole, so
	// without this an update that only meant to touch something else erased it.
	Description types.Null[string] `json:"description"`
}

// upsertNamespace creates or updates a namespace's description record.
func upsertNamespace(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req upsertNamespaceRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		rec, err := mgr.UpsertNamespace(context.WithoutCancel(c.Request.Context()), req.Name, req.Description)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(rec)
	}
}

// deleteNamespace removes a namespace's description record (repos keep the tag).
func deleteNamespace(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")
		if err := mgr.DeleteNamespace(context.WithoutCancel(c.Request.Context()), name); err != nil {
			return c.Err(err)
		}

		return c.SetStatus(http.StatusNoContent).SendJSON(nil)
	}
}

type setRepoNamespaceRequest struct {
	Namespace string `json:"namespace"`
}

// setRepoNamespace moves a repo into a namespace ("" / "default" returns it to
// the default bucket). It only re-tags the record; no rebuild is triggered.
func setRepoNamespace(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		var req setRepoNamespaceRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		repo, err := mgr.SetRepoNamespace(context.WithoutCancel(c.Request.Context()), id, req.Namespace)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(repo)
	}
}

func repoSettings(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		out, err := mgr.RepoSettings(c.Request.Context(), repoID(c.Request))
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(out)
	}
}

// setRepoOverridesRequest replaces a repo's indexing/documentation overrides.
// The payload is the whole override set, not a patch: the UI edits it as one
// form, and a partial merge would make "clear this list" unexpressible.
type setRepoOverridesRequest struct {
	Overrides registry.Overrides `json:"overrides"`
}

func setRepoOverrides(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		var req setRepoOverridesRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		// A change queues a rebuild, so the write must survive the client
		// navigating away mid-save.
		repo, err := mgr.SetRepoOverrides(context.WithoutCancel(c.Request.Context()), id, req.Overrides)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(repo)
	}
}

// activeRepoView is one repo with currently running pipeline steps.
type activeRepoView struct {
	ID      string `json:"id"`
	Running string `json:"running"`
	Status  string `json:"status,omitempty"`
	// Progress carries the live counters and remaining-time estimate of every
	// step that publishes them (docs_index and code_index run in parallel, so
	// there can be several). Web-source scopes ("web:<name>") appear here too,
	// so the Activity page shows their sync the same way.
	Progress []manager.Progress `json:"progress,omitempty"`
}

// listActiveRepos returns only the repos that have running jobs, so the
// Activity page never has to scan every tracked repository.
func listActiveRepos(mgr repoReader) ada.HandlerFunc {
	return func(c *ada.Context) error {
		active := mgr.ActiveRepos()

		views := make([]activeRepoView, 0, len(active))
		for id, running := range active {
			v := activeRepoView{ID: id, Running: running}
			if repo, err := mgr.Repo(c.Request.Context(), id); err == nil && repo != nil {
				v.Status = repo.Status
			}
			v.Progress, _ = mgr.Progress(id)
			views = append(views, v)
		}

		sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })

		return c.SendJSON(views)
	}
}

// listTasks returns the central work-queue snapshot: the concurrency limit,
// live running/queued counters and the queued/running/recent tasks. Powers the
// Activity page's task view.
func listTasks(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		return c.SendJSON(mgr.TaskSnapshot())
	}
}

// clearTaskHistory removes completed, failed and canceled tasks while leaving
// queued and running work untouched.
func clearTaskHistory(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		mgr.ClearTaskHistory()

		return c.SendJSON(mgr.TaskSnapshot())
	}
}

// cancelPendingTasks removes every queued task while running work continues.
func cancelPendingTasks(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		mgr.CancelPendingTasks()

		return c.SendJSON(mgr.TaskSnapshot())
	}
}

type taskConcurrencyRequest struct {
	Limit int `json:"limit"`
}

// setTaskConcurrency changes how many background tasks run at once, effective
// immediately. It returns the resulting snapshot so the UI reflects the new
// limit without a second fetch. The value is not persisted to settings.
func setTaskConcurrency(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req taskConcurrencyRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		mgr.SetTaskConcurrency(req.Limit)

		return c.SendJSON(mgr.TaskSnapshot())
	}
}

// bumpTask moves a queued task to the front of the backlog so it starts next.
func bumpTask(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		seq, err := strconv.ParseUint(c.Request.PathValue("seq"), 10, 64)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "invalid task seq"})
		}

		if !mgr.BumpTask(seq) {
			return c.SetStatus(http.StatusConflict).SendJSON(map[string]string{
				"error": "no queued task with that seq (it may be running or already finished)",
			})
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(mgr.TaskSnapshot())
	}
}

// cancelTask cancels a single task by seq: a queued task is dropped from the
// backlog, while a running task has its underlying job aborted.
func cancelTask(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		seq, err := strconv.ParseUint(c.Request.PathValue("seq"), 10, 64)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "invalid task seq"})
		}

		if !mgr.CancelTask(seq) {
			return c.SetStatus(http.StatusConflict).SendJSON(map[string]string{
				"error": "no task with that seq (it may already be finished)",
			})
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(mgr.TaskSnapshot())
	}
}

func addRepo(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req addRepoRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		if req.URL == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "url is required"})
		}

		if err := validateStages(req.Skip); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		// Registration must finish even if the UI navigates away. The clone/build
		// itself is queued on the manager lifecycle context by AddRepo.
		repo, err := mgr.AddRepo(context.WithoutCancel(c.Request.Context()), manager.RepoSpec{
			URL:       req.URL,
			Branch:    req.Branch,
			Namespace: req.Namespace,
			Overrides: req.Overrides,
		}, req.Skip...)
		if err != nil {
			return c.Err(err)
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(repo)
	}
}

func getRepo(mgr repoReader) ada.HandlerFunc {
	return func(c *ada.Context) error {
		repo, err := mgr.Repo(c.Request.Context(), repoID(c.Request))
		if err != nil {
			return c.Err(err)
		}

		if repo == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		return c.SendJSON(viewRepo(mgr, repo))
	}
}

func deleteRepo(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		if err := mgr.RemoveRepo(context.WithoutCancel(c.Request.Context()), repoID(c.Request)); err != nil {
			return c.Err(err)
		}

		return c.SendNoContent()
	}
}

type refreshRequest struct {
	// Skip names stages this run must not execute: graph, docs, docs_index,
	// code_index. It applies to this refresh only and is additive to the repo's
	// persisted skip_stages override, so a small code change can be pulled in
	// without paying for documentation generation. Skipping docs also skips
	// docs_index, which would otherwise re-embed the previous run's markdown.
	Skip []string `json:"skip,omitempty"`
}

func refreshRepo(mgr repoService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		repo, err := mgr.Repo(context.WithoutCancel(c.Request.Context()), id)
		if err != nil {
			return c.Err(err)
		}

		if repo == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		// The body is optional: a plain refresh has always been a POST with no
		// content and must keep working.
		var req refreshRequest
		if err := bindOptionalJSON(c, &req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		if err := validateStages(req.Skip); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		if err := mgr.TriggerRefresh(id, req.Skip...); err != nil {
			return c.Err(err)
		}

		out := map[string]any{"status": "refresh queued", "repo": id}
		if len(req.Skip) > 0 {
			out["skip"] = req.Skip
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(out)
	}
}

// validateStages rejects unknown stage names so a typo fails with a clear 400
// instead of silently skipping (or building) nothing.
func validateStages(stages []string) error {
	for _, s := range stages {
		if !registry.ValidStage(s) {
			return fmt.Errorf("unknown stage %q (valid: %s, %s, %s, %s)",
				s, registry.StageGraph, registry.StageDocs,
				registry.StageDocsIndex, registry.StageCodeIndex)
		}
	}

	return nil
}

// bindOptionalJSON decodes a request body the caller may legitimately omit. An
// absent or empty body leaves dst at its zero value instead of failing, so an
// endpoint can gain optional parameters without breaking clients that post
// nothing.
func bindOptionalJSON(c *ada.Context, dst any) error {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return nil
	}

	if err := c.Bind(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}

		return err
	}

	return nil
}

type generateRequest struct {
	// Targets selects the stages to run: graph, docs, docs_index, code_index.
	Targets []string `json:"targets"`
	// Force makes the docs stage ignore its incremental caches and regenerate
	// every summary and documentation.md even when nothing changed.
	Force bool `json:"force"`
}

func generateRepo(mgr repoService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		repo, err := mgr.Repo(context.WithoutCancel(c.Request.Context()), id)
		if err != nil {
			return c.Err(err)
		}

		if repo == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		var req generateRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		if len(req.Targets) == 0 {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "targets is required"})
		}

		for _, t := range req.Targets {
			if !registry.ValidStage(t) {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{
					"error": fmt.Sprintf("unknown target %q (valid: graph, docs, docs_index, code_index)", t),
				})
			}
		}

		if err := mgr.TriggerGenerate(id, req.Targets, req.Force); err != nil {
			return c.Err(err)
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(map[string]any{
			"status": "generate queued", "repo": id, "targets": req.Targets, "force": req.Force,
		})
	}
}

// cancelRepoJob aborts the refresh/generate job currently running for a repo.
func cancelRepoJob(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		if !mgr.CancelJob(id) {
			return c.SetStatus(http.StatusConflict).SendJSON(map[string]string{
				"error": "no job running for " + id,
			})
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(map[string]string{"status": "cancelling", "repo": id})
	}
}

// ---- artifact handlers ------------------------------------------------------

// repoArtifact serves a graphify output file (graph.json, GRAPH_REPORT.md,
// graph.html) for a tracked repository so external tools can consume them
// without filesystem access.
func repoArtifact(mgr repoReader, pathFn func(repoPath string) string) ada.HandlerFunc {
	return func(c *ada.Context) error {
		repo, err := mgr.Repo(c.Request.Context(), repoID(c.Request))
		if err != nil {
			return c.Err(err)
		}

		if repo == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "repo not found"})
		}

		path := pathFn(repo.Path)
		if _, err := os.Stat(path); err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{
				"error": "artifact not built yet (status: " + repo.Status + ")",
			})
		}

		http.ServeFile(c.Response, c.Request, path)

		return nil
	}
}

// ---- repo file handlers -----------------------------------------------------

func listRepoFiles(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		subdir := c.Request.URL.Query().Get("subdir")
		snapshot := c.Request.URL.Query().Get("snapshot")
		recursive := c.Request.URL.Query().Get("recursive") == "true"

		entries, token, err := mgr.ListRepoFilesAt(c.Request.Context(), repoID(c.Request), subdir, snapshot, recursive)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}
		c.Response.Header().Set("X-Krabby-Snapshot", token)

		return c.SendJSON(entries)
	}
}

func globRepoFiles(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		params := c.Request.URL.Query()
		pattern := strings.TrimSpace(params.Get("pattern"))
		if pattern == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "pattern query param is required"})
		}

		page, err := mgr.GlobRepoFiles(c.Request.Context(), repoID(c.Request), params.Get("snapshot"), pattern, queryInt(params.Get("limit"), 0))
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}
		c.Response.Header().Set("X-Krabby-Snapshot", page.Snapshot)

		return c.SendJSON(page)
	}
}
func readRepoFile(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		path := c.Request.URL.Query().Get("path")
		if path == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "path query param is required"})
		}

		var offset int64
		if v := c.Request.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				offset = n
			}
		}

		var maxBytes int
		if v := c.Request.URL.Query().Get("max_bytes"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				maxBytes = n
			}
		}

		fc, err := mgr.ReadRepoFileAt(c.Request.Context(), repoID(c.Request), path, c.Request.URL.Query().Get("snapshot"), offset, maxBytes)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(fc)
	}
}

// ---- docs + RAG handlers ----------------------------------------------------

func listDocs(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		docs, err := mgr.ListDocs(c.Request.Context(), repoID(c.Request))
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(docs)
	}
}

func getDoc(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		path := c.Request.URL.Query().Get("path")
		if path == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "path query param is required"})
		}

		doc, err := mgr.GetDoc(c.Request.Context(), repoID(c.Request), path, 0, 0)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(doc)
	}
}

func searchDocs(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := c.Request.URL.Query().Get("q")
		if q == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "q query param is required"})
		}

		// repo may be a repository id, a web-source key ("web:<name>") or an
		// API-catalog key ("api:<name>") and wins over scope; scope selects
		// all/repos/sources/apis when repo is empty.
		repo := c.Request.URL.Query().Get("repo")
		scope := c.Request.URL.Query().Get("scope")
		namespace := namespaceParam(c.Request)
		mode := c.Request.URL.Query().Get("mode")

		var top int
		if v := c.Request.URL.Query().Get("top"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				top = n
			}
		}

		docs, err := mgr.SearchDocs(c.Request.Context(), scope, repo, namespace, mode, q, top)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(docs)
	}
}

func searchCode(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := strings.TrimSpace(c.Request.URL.Query().Get("q"))
		if q == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "q query param is required"})
		}

		params := c.Request.URL.Query()
		repo := params.Get("repo") // "" = all repos
		namespace := namespaceParam(c.Request)
		mode := params.Get("mode")
		if mode == "" {
			mode = "normal"
		}

		switch mode {
		case "normal":
			result, err := mgr.SearchCodeText(c.Request.Context(), repo, namespace, q, coderag.TextSearchOptions{
				Page:    queryInt(params.Get("page"), 1),
				PerPage: queryInt(params.Get("per_page"), 20),
				Path:    params.Get("path"),
			})
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SendJSON(result)
		case "regex":
			opts := coderag.RegexOptions{
				CaseSensitive: params.Get("case_sensitive") == "true",
				Path:          params.Get("path"),
				Page:          queryInt(params.Get("page"), 1),
				PerPage:       queryInt(params.Get("per_page"), 20),
			}
			if n, err := strconv.Atoi(params.Get("context_lines")); err == nil {
				opts.ContextLines = n
			}
			if n, err := strconv.Atoi(params.Get("max_matches")); err == nil {
				opts.MaxMatches = n
			}

			result, err := mgr.SearchCodeRegex(c.Request.Context(), repo, namespace, q, opts)
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SendJSON(result)
		case "semantic":
			page, err := mgr.SearchCode(c.Request.Context(), repo, namespace, q, coderag.SemanticOptions{
				TopK: queryInt(params.Get("top"), 0),
				Path: params.Get("path"),
			})
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SendJSON(page)
		default:
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{
				"error": "mode must be normal, regex or semantic",
			})
		}
	}
}

func getDocsConfig(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		cfg, err := mgr.GetDocsConfig(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(cfg)
	}
}

func setDocsConfig(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if err := c.Bind(&patch); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		cfg, err := mgr.PatchDocsConfig(c.Request.Context(), patch)
		if err != nil {
			// Settings were saved but the client rebuild failed: report the
			// error while still returning the redacted (persisted) config.
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]any{
				"error":  err.Error(),
				"config": cfg,
			})
		}

		return c.SendJSON(cfg)
	}
}

func testLLM(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if c.Request.ContentLength != 0 {
			if err := c.Bind(&patch); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		merged, err := applySettingsPatch(c.Request.Context(), mgr, patch)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(mgr.TestLLM(c.Request.Context(), merged))
	}
}

func testEmbedder(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if c.Request.ContentLength != 0 {
			if err := c.Bind(&patch); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		merged, err := applySettingsPatch(c.Request.Context(), mgr, patch)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(mgr.TestEmbedder(c.Request.Context(), merged))
	}
}

func testLangfuse(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if c.Request.ContentLength != 0 {
			if err := c.Bind(&patch); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		merged, err := applySettingsPatch(c.Request.Context(), mgr, patch)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(mgr.TestLangfuse(c.Request.Context(), merged))
	}
}

func testCodeEmbedder(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if c.Request.ContentLength != 0 {
			if err := c.Bind(&patch); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		merged, err := applySettingsPatch(c.Request.Context(), mgr, patch)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(mgr.TestCodeEmbedder(c.Request.Context(), merged))
	}
}

func applySettingsPatch(ctx context.Context, mgr docsSettingsService, patch settings.Patch) (settings.Settings, error) {
	current, err := mgr.GetDocsConfig(ctx)
	if err != nil {
		return settings.Settings{}, err
	}

	return patch.Apply(current.Settings), nil
}

func mergedGraph(mgr docsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := mgr.MergedPath()
		if path == "" {
			http.Error(w, "merged graph not built yet", http.StatusNotFound)

			return
		}

		http.ServeFile(w, r, path)
	}
}

// ---- credential handlers ----------------------------------------------------

type setCredentialRequest struct {
	Pattern  string `json:"pattern"`
	Kind     string `json:"kind"`
	Username string `json:"username"`
	Secret   string `json:"secret"`
}

func listCredentials(mgr credentialService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		creds, err := mgr.ListCredentials(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		// Credential.Secret carries json:"-"; secrets never leave the server.
		return c.SendJSON(creds)
	}
}

func setCredential(mgr credentialService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req setCredentialRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		cred := &credentials.Credential{
			Pattern:  req.Pattern,
			Kind:     req.Kind,
			Username: req.Username,
			Secret:   req.Secret,
		}
		if err := mgr.SetCredential(c.Request.Context(), cred); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SetStatus(http.StatusCreated).SendJSON(cred)
	}
}

func deleteCredential(mgr credentialService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		pattern := c.Request.URL.Query().Get("pattern")
		if pattern == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "pattern query param is required"})
		}

		if err := mgr.DeleteCredential(c.Request.Context(), pattern); err != nil {
			return c.Err(err)
		}

		return c.SendNoContent()
	}
}

// ---- provider-neutral git webhook ------------------------------------------

// gitPushEvent covers the repository identity fields used by GitHub, GitLab,
// Gitea and compatible servers. Unknown fields are intentionally ignored.
type gitPushEvent struct {
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
		GitHTTPURL        string `json:"git_http_url"`
		GitSSHURL         string `json:"git_ssh_url"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	RepositoryURL string `json:"repository_url"`
}

func gitWebhook(mgr webhookService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)

			return
		}

		// Resolve per request so a secret changed through UI/REST applies
		// immediately without rebuilding the HTTP routes or restarting.
		if secret := mgr.WebhookSecret(); secret != "" {
			if !verifyGitWebhook(secret, body, r.Header) {
				http.Error(w, "invalid signature", http.StatusUnauthorized)

				return
			}
		}

		var event gitPushEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)

			return
		}

		ref := gitEventRepoRef(event)
		if ref == "" {
			http.Error(w, "payload has no repository identity", http.StatusBadRequest)

			return
		}

		repo, err := mgr.ResolveRepo(r.Context(), ref)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		if repo == nil {
			http.Error(w, "repo not tracked", http.StatusNotFound)

			return
		}

		if err := mgr.TriggerRefresh(repo.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"status":"refresh queued","repo":%q}`, repo.ID)
	}
}

// gitEventRepoRef prefers clone/web URLs because ParseRepoID preserves the git
// server host. Provider path-only fields are a fallback and resolve by unique
// suffix when the payload does not expose a URL.
func gitEventRepoRef(event gitPushEvent) string {
	urls := []string{
		event.Repository.CloneURL,
		event.Repository.SSHURL,
		event.Project.GitHTTPURL,
		event.Project.GitSSHURL,
		event.RepositoryURL,
		event.Repository.HTMLURL,
		event.Project.WebURL,
	}
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		if id, err := gitops.ParseRepoID(raw); err == nil {
			return id
		}
	}

	if event.Project.PathWithNamespace != "" {
		return event.Project.PathWithNamespace
	}

	return event.Repository.FullName
}

// verifyGitWebhook first accepts provider-neutral shared-token headers, then
// the common authentication schemes used by popular git servers. A custom git
// server can always send Authorization: Bearer <secret> or X-Webhook-Token.
func verifyGitWebhook(secret string, body []byte, header http.Header) bool {
	tokens := []string{
		header.Get("X-Webhook-Token"),
		strings.TrimPrefix(header.Get("Authorization"), "Bearer "),
		header.Get("X-Gitlab-Token"),
	}
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1 {
			return true
		}
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	for _, name := range []string{"X-Hub-Signature-256", "X-Gitea-Signature", "X-Gogs-Signature"} {
		sig := strings.TrimPrefix(header.Get(name), "sha256=")
		if sig != "" && hmac.Equal([]byte(expected), []byte(sig)) {
			return true
		}
	}

	return false
}
