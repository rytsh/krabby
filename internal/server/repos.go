package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/registry"
)

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

// repoArtifact serves a graph output file (graph.json, GRAPH_REPORT.md,
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
