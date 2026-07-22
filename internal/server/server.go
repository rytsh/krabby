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
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rakunlabs/ada"
	mcors "github.com/rakunlabs/ada/middleware/cors"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"
	mserver "github.com/rakunlabs/ada/middleware/server"
	mtelemetry "github.com/rakunlabs/ada/middleware/telemetry"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/lease"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/settings"
)

// Start runs the HTTP server until ctx is cancelled.
func Start(ctx context.Context, cfg *config.Config, mgr *manager.Manager, mcpServer *mcp.Server) error {
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

	// MCP endpoint (streamable HTTP). POST/GET/DELETE share the same path.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{},
	)
	// The MCP key can be overridden at runtime from the UI; resolve it per
	// request through the manager's cached value.
	mgr.InitMCPKey(ctx, cfg.MCP.APIKey)
	base.Handle(cfg.MCP.Path, mcpHandler, apiKeyMiddleware(mgr.MCPAPIKey))

	api := base.Group("/api/v1")
	api.GET("/settings", server.Wrap(getSettings(cfg, mgr)))
	api.GET("/mcp/api-key", server.Wrap(getMCPKey(mgr)))
	api.PUT("/mcp/api-key", server.Wrap(setMCPKey(mgr)))
	api.DELETE("/mcp/api-key", server.Wrap(clearMCPKey(mgr)))
	api.GET("/repos", server.Wrap(listRepos(mgr)))
	api.GET("/repos/owners", server.Wrap(listRepoOwners(mgr)))
	api.GET("/repos/active", server.Wrap(listActiveRepos(mgr)))
	api.POST("/repos", server.Wrap(addRepo(mgr)))

	// Repo ids are full paths (host/group/.../name) with any number of "/"
	// segments, so repo-scoped routes use a greedy wildcard and a GitLab-style
	// "/-/" separator between the id and the action:
	//   GET    /repos/<id>                  repo record
	//   POST   /repos/<id>/-/refresh        queue refresh
	//   GET    /repos/<id>/-/files          list clone files
	//   ...
	api.GET("/repos/{ref...}", server.Wrap(dispatchRepo(mgr, map[string]ada.HandlerFunc{
		"":       getRepo(mgr),
		"lock":   lockStatus(mgr),
		"graph":  repoArtifact(mgr, graphify.GraphPath),
		"report": repoArtifact(mgr, graphify.ReportPath),
		"html":   repoArtifact(mgr, graphify.HTMLPath),
		"files":  listRepoFiles(mgr),
		"file":   readRepoFile(mgr),
		"docs":   listDocs(mgr),
		"doc":    getDoc(mgr),
	})))
	api.POST("/repos/{ref...}", server.Wrap(dispatchRepo(mgr, map[string]ada.HandlerFunc{
		"refresh":  refreshRepo(mgr),
		"generate": generateRepo(mgr),
		"cancel":   cancelRepoJob(mgr),
		"lock":     lockRepo(mgr),
	})))
	api.DELETE("/repos/{ref...}", server.Wrap(dispatchRepo(mgr, map[string]ada.HandlerFunc{
		"":     deleteRepo(mgr),
		"lock": unlockRepo(mgr),
	})))
	// Web content sources (wikis, Confluence spaces): named collections whose
	// pages are synced to markdown and indexed into the docs RAG.
	api.GET("/sources", server.Wrap(listSources(mgr)))
	api.POST("/sources", server.Wrap(addSource(mgr)))
	api.GET("/sources/{name}", server.Wrap(getSource(mgr)))
	api.PUT("/sources/{name}", server.Wrap(updateSource(mgr)))
	api.DELETE("/sources/{name}", server.Wrap(deleteSource(mgr)))
	api.POST("/sources/{name}/refresh", server.Wrap(refreshSource(mgr)))
	api.POST("/sources/{name}/pages", server.Wrap(addSourcePage(mgr)))
	api.DELETE("/sources/{name}/pages", server.Wrap(deleteSourcePage(mgr)))
	api.GET("/sources/{name}/doc", server.Wrap(getSourceDoc(mgr)))

	api.GET("/docs/search", server.Wrap(searchDocs(mgr)))
	api.GET("/code/search", server.Wrap(searchCode(mgr)))
	api.GET("/docs/config", server.Wrap(getDocsConfig(mgr)))
	api.PUT("/docs/config", server.Wrap(setDocsConfig(mgr)))
	api.POST("/docs/config/test/llm", server.Wrap(testLLM(mgr)))
	api.POST("/docs/config/test/embedder", server.Wrap(testEmbedder(mgr)))
	api.POST("/docs/config/test/code-embedder", server.Wrap(testCodeEmbedder(mgr)))
	api.GET("/graph", mergedGraph(mgr))
	api.GET("/credentials", server.Wrap(listCredentials(mgr)))
	api.PUT("/credentials", server.Wrap(setCredential(mgr)))
	api.DELETE("/credentials", server.Wrap(deleteCredential(mgr)))

	base.POST("/webhook/git", gitWebhook(mgr))

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

	return server.StartWithContext(ctx, cfg.Server.Host+":"+cfg.Server.Port)
}

// ---- middleware -------------------------------------------------------------

// apiKeyMiddleware guards a handler with an API key resolved per request, so
// runtime changes (UI-managed MCP key) apply without a restart. An empty key
// means the endpoint is open.
func apiKeyMiddleware(getKey func() string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := getKey()
			if apiKey == "" {
				next.ServeHTTP(w, r)

				return
			}

			got := r.Header.Get("X-Api-Key")
			if got == "" {
				got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}

			if subtle.ConstantTimeCompare([]byte(got), []byte(apiKey)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ---- settings handler -------------------------------------------------------

// settingsResponse is a redacted view of the running config for the UI. Secrets
// (MCP api key, webhook secret) are deliberately omitted; booleans indicate
// only whether they are configured.
type settingsResponse struct {
	Version  string `json:"version"`
	LogLevel string `json:"log_level"`
	DataDir  string `json:"data_dir"`

	Server struct {
		Host     string `json:"host"`
		Port     string `json:"port"`
		BasePath string `json:"base_path"`
	} `json:"server"`

	MCP struct {
		Path      string `json:"path"`
		APIKeySet bool   `json:"api_key_set"`
	} `json:"mcp"`

	Graphify struct {
		Bin          string `json:"bin"`
		Python       string `json:"python,omitempty"`
		BuildTimeout string `json:"build_timeout"`
	} `json:"graphify"`
}

func getSettings(cfg *config.Config, mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var s settingsResponse

		s.Version = config.Version
		s.LogLevel = cfg.LogLevel
		s.DataDir = cfg.DataDir

		s.Server.Host = cfg.Server.Host
		s.Server.Port = cfg.Server.Port
		s.Server.BasePath = cfg.Server.BasePath

		s.MCP.Path = cfg.MCP.Path
		s.MCP.APIKeySet = mgr.MCPAPIKey() != ""

		s.Graphify.Bin = cfg.Graphify.Bin
		s.Graphify.Python = cfg.Graphify.Python
		s.Graphify.BuildTimeout = cfg.Graphify.BuildTimeout.String()

		return c.SendJSON(s)
	}
}

// ---- MCP api key handlers ---------------------------------------------------

type mcpKeyRequest struct {
	APIKey string `json:"api_key"`
}

func getMCPKey(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		return c.SendJSON(map[string]bool{"api_key_set": mgr.MCPAPIKey() != ""})
	}
}

// setMCPKey stores a runtime override for the MCP API key and applies it
// immediately. An empty api_key disables authentication.
func setMCPKey(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req mcpKeyRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		if err := mgr.SetMCPAPIKey(c.Request.Context(), strings.TrimSpace(req.APIKey)); err != nil {
			return c.Err(err)
		}

		return c.SendJSON(map[string]bool{"api_key_set": mgr.MCPAPIKey() != ""})
	}
}

// clearMCPKey removes the runtime override so the file/env config value
// applies again.
func clearMCPKey(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		if err := mgr.ClearMCPAPIKey(c.Request.Context()); err != nil {
			return c.Err(err)
		}

		return c.SendJSON(map[string]bool{"api_key_set": mgr.MCPAPIKey() != ""})
	}
}

// ---- REST handlers ----------------------------------------------------------

type addRepoRequest struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
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

// dispatchRepo routes /repos/{ref...} requests: it splits "<id>[/-/<action>]",
// resolves the id (exact match or unique legacy-suffix match) to its canonical
// form, and invokes the handler registered for the action.
func dispatchRepo(mgr *manager.Manager, routes map[string]ada.HandlerFunc) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id, action := repoRef(c.Request)
		if id == "" {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "repo id is required"})
		}

		handler, ok := routes[action]
		if !ok {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": fmt.Sprintf("unknown repo action %q", action)})
		}

		repo, err := mgr.Registry().Resolve(c.Request.Context(), id)
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

func viewRepo(mgr *manager.Manager, repo *registry.Repo) repoView {
	return repoView{Repo: repo, Running: mgr.Activity(repo.ID)}
}

// pagedRepos is the paginated envelope returned by GET /repos.
type pagedRepos struct {
	Items   []repoView `json:"items"`
	Total   int        `json:"total"`
	Page    int        `json:"page"`
	PerPage int        `json:"per_page"`
}

func listRepos(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		params := c.Request.URL.Query()

		opts := registry.ListOptions{
			Search: params.Get("q"),
			Owner:  params.Get("owner"),
		}
		if n, err := strconv.Atoi(params.Get("page")); err == nil && n > 0 {
			opts.Page = n
		}
		if n, err := strconv.Atoi(params.Get("per_page")); err == nil && n > 0 {
			opts.PerPage = n
		}

		repos, total, err := mgr.Registry().ListPaged(c.Request.Context(), opts)
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
func listRepoOwners(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		owners, err := mgr.Registry().Owners(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(owners)
	}
}

// activeRepoView is one repo with currently running pipeline steps.
type activeRepoView struct {
	ID      string `json:"id"`
	Running string `json:"running"`
	Status  string `json:"status,omitempty"`
}

// listActiveRepos returns only the repos that have running jobs, so the
// Activity page never has to scan every tracked repository.
func listActiveRepos(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		active := mgr.ActiveRepos()

		views := make([]activeRepoView, 0, len(active))
		for id, running := range active {
			v := activeRepoView{ID: id, Running: running}
			if repo, err := mgr.Registry().Get(c.Request.Context(), id); err == nil && repo != nil {
				v.Status = repo.Status
			}
			views = append(views, v)
		}

		sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })

		return c.SendJSON(views)
	}
}

func addRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req addRepoRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		if req.URL == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "url is required"})
		}

		// Registration must finish even if the UI navigates away. The clone/build
		// itself is queued on the manager lifecycle context by AddRepo.
		repo, err := mgr.AddRepo(context.WithoutCancel(c.Request.Context()), req.URL, req.Branch)
		if err != nil {
			return c.Err(err)
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(repo)
	}
}

func getRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		repo, err := mgr.Registry().Get(c.Request.Context(), repoID(c.Request))
		if err != nil {
			return c.Err(err)
		}

		if repo == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		return c.SendJSON(viewRepo(mgr, repo))
	}
}

func deleteRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		if err := mgr.RemoveRepo(context.WithoutCancel(c.Request.Context()), repoID(c.Request)); err != nil {
			return c.Err(err)
		}

		return c.SendNoContent()
	}
}

func refreshRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		repo, err := mgr.Registry().Get(context.WithoutCancel(c.Request.Context()), id)
		if err != nil {
			return c.Err(err)
		}

		if repo == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		mgr.TriggerRefresh(id)

		return c.SetStatus(http.StatusAccepted).SendJSON(map[string]string{"status": "refresh queued", "repo": id})
	}
}

type generateRequest struct {
	// Targets selects the stages to run: graph, docs, docs_index, code_index.
	Targets []string `json:"targets"`
	// Force makes the docs stage ignore its incremental caches and regenerate
	// every summary and documentation.md even when nothing changed.
	Force bool `json:"force"`
}

func generateRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		repo, err := mgr.Registry().Get(context.WithoutCancel(c.Request.Context()), id)
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

		mgr.TriggerGenerate(id, req.Targets, req.Force)

		return c.SetStatus(http.StatusAccepted).SendJSON(map[string]any{
			"status": "generate queued", "repo": id, "targets": req.Targets, "force": req.Force,
		})
	}
}

// cancelRepoJob aborts the refresh/generate job currently running for a repo.
func cancelRepoJob(mgr *manager.Manager) ada.HandlerFunc {
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

// ---- lease handlers ---------------------------------------------------------

type lockRequest struct {
	Owner string `json:"owner"`
	TTL   string `json:"ttl"` // Go duration, e.g. "5m"; empty = default
}

func lockRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req lockRequest
		if c.Request.ContentLength > 0 {
			if err := c.Bind(&req); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		var ttl time.Duration

		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "invalid ttl: " + err.Error()})
			}

			ttl = d
		}

		l, err := mgr.AcquireLease(c.Request.Context(), repoID(c.Request), req.Owner, ttl)
		if err != nil {
			if errors.Is(err, lease.ErrLeased) {
				return c.SetStatus(http.StatusConflict).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		// Token is included: the caller needs it to release the lock.
		return c.SendJSON(l)
	}
}

func lockStatus(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		l := mgr.LeaseInfo(repoID(c.Request))
		if l == nil {
			return c.SendJSON(map[string]any{"locked": false})
		}

		return c.SendJSON(map[string]any{"locked": true, "lease": l})
	}
}

func unlockRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		token := c.Request.Header.Get("X-Lock-Token")
		if token == "" {
			token = c.Request.URL.Query().Get("token")
		}

		if token == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "X-Lock-Token header (or token query param) is required"})
		}

		if err := mgr.ReleaseLease(repoID(c.Request), token); err != nil {
			switch {
			case errors.Is(err, lease.ErrNotLeased):
				return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
			case errors.Is(err, lease.ErrBadToken):
				return c.SetStatus(http.StatusForbidden).SendJSON(map[string]string{"error": err.Error()})
			default:
				return c.Err(err)
			}
		}

		return c.SendNoContent()
	}
}

// ---- artifact handlers ------------------------------------------------------

// repoArtifact serves a graphify output file (graph.json, GRAPH_REPORT.md,
// graph.html) for a tracked repository so external tools can consume them
// without filesystem access.
func repoArtifact(mgr *manager.Manager, pathFn func(repoPath string) string) ada.HandlerFunc {
	return func(c *ada.Context) error {
		repo, err := mgr.Registry().Get(c.Request.Context(), repoID(c.Request))
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

func listRepoFiles(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		subdir := c.Request.URL.Query().Get("subdir")
		recursive := c.Request.URL.Query().Get("recursive") == "true"

		entries, err := mgr.ListRepoFiles(c.Request.Context(), repoID(c.Request), subdir, recursive)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(entries)
	}
}

func readRepoFile(mgr *manager.Manager) ada.HandlerFunc {
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

		fc, err := mgr.ReadRepoFile(c.Request.Context(), repoID(c.Request), path, offset, maxBytes)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(fc)
	}
}

// ---- docs + RAG handlers ----------------------------------------------------

func listDocs(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		docs, err := mgr.ListDocs(c.Request.Context(), repoID(c.Request))
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(docs)
	}
}

func getDoc(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		path := c.Request.URL.Query().Get("path")
		if path == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "path query param is required"})
		}

		doc, err := mgr.GetDoc(c.Request.Context(), repoID(c.Request), path)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(doc)
	}
}

func searchDocs(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := c.Request.URL.Query().Get("q")
		if q == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "q query param is required"})
		}

		// repo may be a repository id or a web-source key ("web:<name>") and
		// wins over scope; scope selects all/repos/sources when repo is empty.
		repo := c.Request.URL.Query().Get("repo")
		scope := c.Request.URL.Query().Get("scope")

		var top int
		if v := c.Request.URL.Query().Get("top"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				top = n
			}
		}

		docs, err := mgr.SearchDocs(c.Request.Context(), scope, repo, q, top)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(docs)
	}
}

func searchCode(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := strings.TrimSpace(c.Request.URL.Query().Get("q"))
		if q == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "q query param is required"})
		}

		params := c.Request.URL.Query()
		repo := params.Get("repo") // "" = all repos
		mode := params.Get("mode")
		if mode == "" {
			mode = "normal"
		}

		switch mode {
		case "normal":
			page, perPage := 1, 20
			if n, err := strconv.Atoi(params.Get("page")); err == nil && n > 0 {
				page = n
			}
			if n, err := strconv.Atoi(params.Get("per_page")); err == nil && n > 0 {
				perPage = n
			}

			result, err := mgr.SearchCodeText(c.Request.Context(), repo, q, page, perPage)
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SendJSON(result)
		case "semantic":
			var top int
			if v := params.Get("top"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					top = n
				}
			}

			snippets, err := mgr.SearchCode(c.Request.Context(), repo, q, top)
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SendJSON(map[string]any{
				"results":  snippets,
				"total":    len(snippets),
				"page":     1,
				"per_page": len(snippets),
			})
		default:
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{
				"error": "mode must be normal or semantic",
			})
		}
	}
}

func getDocsConfig(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		cfg, err := mgr.GetDocsConfig(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(cfg)
	}
}

func setDocsConfig(mgr *manager.Manager) ada.HandlerFunc {
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

func testLLM(mgr *manager.Manager) ada.HandlerFunc {
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

func testEmbedder(mgr *manager.Manager) ada.HandlerFunc {
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

func testCodeEmbedder(mgr *manager.Manager) ada.HandlerFunc {
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

func applySettingsPatch(ctx context.Context, mgr *manager.Manager, patch settings.Patch) (settings.Settings, error) {
	current, err := mgr.GetDocsConfig(ctx)
	if err != nil {
		return settings.Settings{}, err
	}

	return patch.Apply(current.Settings), nil
}

func mergedGraph(mgr *manager.Manager) http.HandlerFunc {
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

func listCredentials(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		creds, err := mgr.Credentials().List(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		// Credential.Secret carries json:"-"; secrets never leave the server.
		return c.SendJSON(creds)
	}
}

func setCredential(mgr *manager.Manager) ada.HandlerFunc {
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
		if err := mgr.Credentials().Set(c.Request.Context(), cred); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SetStatus(http.StatusCreated).SendJSON(cred)
	}
}

func deleteCredential(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		pattern := c.Request.URL.Query().Get("pattern")
		if pattern == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "pattern query param is required"})
		}

		if err := mgr.Credentials().Delete(c.Request.Context(), pattern); err != nil {
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

func gitWebhook(mgr *manager.Manager) http.HandlerFunc {
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

		repo, err := mgr.Registry().Resolve(r.Context(), ref)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		if repo == nil {
			http.Error(w, "repo not tracked", http.StatusNotFound)

			return
		}

		mgr.TriggerRefresh(repo.ID)

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
