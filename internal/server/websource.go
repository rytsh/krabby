package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/websource"
)

// ---- web source handlers ----------------------------------------------------

// sourceRequest is the create/update payload for a collection. The API token
// is write-only: an empty value keeps the stored token on update.
type sourceRequest struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	Description     string `json:"description"`
	RefreshInterval string `json:"refresh_interval"` // Go duration, e.g. "24h"; empty = manual only. Used only when Specs is empty.

	// Specs are cron schedules (hardloop syntax, e.g. "0 2 * * *" or "@every
	// 6h") on which the scheduler re-syncs the source, mirroring repository
	// schedules. Authoritative over RefreshInterval when set.
	Specs []string `json:"specs"`

	// Config is an opaque provider-owned object. The registered fetcher
	// validates, merges and redacts it.
	Config json.RawMessage `json:"config"`
}

func (r sourceRequest) collection() (*websource.Collection, error) {
	specs := make([]string, 0, len(r.Specs))
	for _, s := range r.Specs {
		if s = strings.TrimSpace(s); s != "" {
			specs = append(specs, s)
		}
	}

	col := &websource.Collection{
		Name:        strings.TrimSpace(strings.ToLower(r.Name)),
		Type:        strings.TrimSpace(r.Type),
		Description: strings.TrimSpace(r.Description),
		Config:      r.Config,
		Specs:       specs,
	}

	if r.RefreshInterval != "" {
		d, err := time.ParseDuration(r.RefreshInterval)
		if err != nil {
			return nil, err
		}

		col.RefreshInterval = d
	}

	return col, nil
}

// sourceView is the REST shape of a collection: secrets are reduced to a
// set/unset boolean, the refresh interval is a duration string and the scope
// key + live activity are included for the UI.
type sourceView struct {
	*websource.Collection
	RefreshInterval string            `json:"refresh_interval"`
	Config          any               `json:"config,omitempty"`
	ScopeKey        string            `json:"scope_key"`
	PageCount       int               `json:"page_count"`
	Running         string            `json:"running,omitempty"`
	Progress        *manager.Progress `json:"progress,omitempty"`
}

func viewSource(mgr *manager.Manager, col *websource.Collection, pageCount int) sourceView {
	interval := ""
	if col.RefreshInterval > 0 {
		interval = col.RefreshInterval.String()
	}

	scope := websource.ScopeKey(col.Name)
	var progress *manager.Progress
	if p, ok := mgr.Progress(scope); ok {
		progress = &p
	}

	return sourceView{
		Collection:      col,
		RefreshInterval: interval,
		Config:          mgr.WebSourceConfigView(col),
		ScopeKey:        scope,
		PageCount:       pageCount,
		Running:         mgr.Activity(scope),
		Progress:        progress,
	}
}

func listSources(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		cols, err := mgr.ListWebCollections(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		views := make([]sourceView, 0, len(cols))
		for _, col := range cols {
			count, err := mgr.WebPageCount(c.Request.Context(), col.Name)
			if err != nil {
				return c.Err(err)
			}

			views = append(views, viewSource(mgr, col, count))
		}

		return c.SendJSON(views)
	}
}

func addSource(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req sourceRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		col, err := req.collection()
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		if err := mgr.AddWebCollection(c.Request.Context(), col); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(viewSource(mgr, col, 0))
	}
}

// getSource returns a source plus one page of its items. Items are paged at the
// store level (?page, ?per_page) and optionally filtered by ?team, so a large
// source (thousands of pages) is never loaded whole. The response carries the
// total matching count and the paging cursor for the UI.
func getSource(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")

		col, err := mgr.WebCollection(c.Request.Context(), name)
		if err != nil {
			return c.Err(err)
		}

		if col == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		page := queryInt(c.Request.URL.Query().Get("page"), 1)
		perPage := queryInt(c.Request.URL.Query().Get("per_page"), 50)
		if page < 1 {
			page = 1
		}
		if perPage < 1 {
			perPage = 50
		}
		if perPage > 200 {
			perPage = 200
		}

		team := c.Request.URL.Query().Get("team")

		pages, total, err := mgr.WebPagesPaged(c.Request.Context(), name, team, (page-1)*perPage, perPage)
		if err != nil {
			return c.Err(err)
		}

		// The distinct team list (for the UI filter) is meaningful only for
		// jira sources, which are comparatively small; skip the full scan for
		// large discovery sources like Confluence.
		var teams []string
		if col.Type == websource.TypeJira {
			teams, err = mgr.WebSourceTeams(c.Request.Context(), name)
			if err != nil {
				return c.Err(err)
			}
		}

		return c.SendJSON(map[string]any{
			"source":   viewSource(mgr, col, total),
			"pages":    pages,
			"teams":    teams,
			"total":    total,
			"page":     page,
			"per_page": perPage,
			"has_more": page*perPage < total,
		})
	}
}

// queryInt parses a query-param integer, returning def when empty or invalid.
func queryInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}

	return n
}

func updateSource(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req sourceRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		req.Name = c.Request.PathValue("name")

		col, err := req.collection()
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		if err := mgr.UpdateWebCollection(c.Request.Context(), col); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(viewSource(mgr, col, 0))
	}
}

func deleteSource(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")

		if err := mgr.DeleteWebCollection(c.Request.Context(), name); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendNoContent()
	}
}

func refreshSource(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")

		col, err := mgr.WebCollection(c.Request.Context(), name)
		if err != nil {
			return c.Err(err)
		}

		if col == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		mgr.TriggerWebRefresh(name)

		return c.SetStatus(http.StatusAccepted).SendJSON(map[string]string{"status": "refresh queued", "source": name})
	}
}

type addPageRequest struct {
	URL string `json:"url"`
}

func addSourcePage(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req addPageRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		page, err := mgr.AddWebPage(c.Request.Context(), c.Request.PathValue("name"), req.URL)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(page)
	}
}

func deleteSourcePage(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		slug := c.Request.URL.Query().Get("slug")
		if slug == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "slug query param is required"})
		}

		if err := mgr.DeleteWebPage(c.Request.Context(), c.Request.PathValue("name"), slug); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendNoContent()
	}
}

func getSourceDoc(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		path := c.Request.URL.Query().Get("path")
		if path == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "path query param is required"})
		}

		doc, err := mgr.WebSourceDoc(c.Request.Context(), c.Request.PathValue("name"), path)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(doc)
	}
}
