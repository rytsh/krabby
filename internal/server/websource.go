package server

import (
	"encoding/json"
	"net/http"
	"sort"
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
	RefreshInterval string `json:"refresh_interval"` // Go duration, e.g. "24h"; empty = manual only

	// Config is an opaque provider-owned object. The registered fetcher
	// validates, merges and redacts it.
	Config json.RawMessage `json:"config"`
}

func (r sourceRequest) collection() (*websource.Collection, error) {
	col := &websource.Collection{
		Name:        strings.TrimSpace(strings.ToLower(r.Name)),
		Type:        strings.TrimSpace(r.Type),
		Description: strings.TrimSpace(r.Description),
		Config:      r.Config,
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
	RefreshInterval string `json:"refresh_interval"`
	Config          any    `json:"config,omitempty"`
	ScopeKey        string `json:"scope_key"`
	PageCount       int    `json:"page_count"`
	Running         string `json:"running,omitempty"`
}

func viewSource(mgr *manager.Manager, col *websource.Collection, pageCount int) sourceView {
	interval := ""
	if col.RefreshInterval > 0 {
		interval = col.RefreshInterval.String()
	}

	return sourceView{
		Collection:      col,
		RefreshInterval: interval,
		Config:          mgr.WebSourceConfigView(col),
		ScopeKey:        websource.ScopeKey(col.Name),
		PageCount:       pageCount,
		Running:         mgr.Activity(websource.ScopeKey(col.Name)),
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
			pages, err := mgr.WebPages(c.Request.Context(), col.Name)
			if err != nil {
				return c.Err(err)
			}

			views = append(views, viewSource(mgr, col, len(pages)))
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

		allPages, err := mgr.WebPages(c.Request.Context(), name)
		if err != nil {
			return c.Err(err)
		}

		// Optional ?team= filters the listed tickets by team (JIRA sources).
		pages := allPages
		if team := c.Request.URL.Query().Get("team"); team != "" {
			pages, err = mgr.WebPagesByTeam(c.Request.Context(), name, team)
			if err != nil {
				return c.Err(err)
			}
		}

		return c.SendJSON(map[string]any{
			"source": viewSource(mgr, col, len(allPages)),
			"pages":  pages,
			"teams":  collectTeams(allPages),
		})
	}
}

// collectTeams returns the distinct team names across a collection's pages,
// sorted, so the UI can offer a team filter for JIRA sources.
func collectTeams(pages []*websource.Page) []string {
	seen := map[string]string{} // lowercase -> original
	for _, p := range pages {
		for _, t := range p.Teams {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			key := strings.ToLower(t)
			if _, ok := seen[key]; !ok {
				seen[key] = t
			}
		}
	}

	out := make([]string, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)

	return out
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
