package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/manager"
)

// ---- API catalog handlers ---------------------------------------------------

// apiServiceRequest is the create payload for a service. Creation states every
// field; updates use apiServiceUpdateRequest and state only what changes.
type apiServiceRequest struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Group       string `json:"group"`
	Description string `json:"description"`
	BaseURL     string `json:"base_url"`

	RefreshInterval string   `json:"refresh_interval"` // Go duration; empty = manual only. Used only when Specs is empty.
	Specs           []string `json:"specs"`

	// SpecPatch is an RFC 7386 JSON Merge Patch applied to the fetched document
	// before parsing.
	SpecPatch json.RawMessage `json:"spec_patch"`
	// Operations holds per-operation overrides keyed by operation id.
	Operations map[string]apicatalog.OperationOverride `json:"operations"`

	// Config is an opaque provider-owned object. The registered provider
	// validates, merges and redacts it.
	Config json.RawMessage `json:"config"`
}

// apiServiceUpdateRequest is the partial update payload. Every envelope field
// is nullable so omit/null/value are distinguishable, the same rule the
// provider config uses.
type apiServiceUpdateRequest struct {
	apicatalog.ServiceUpdate

	// Config is merged by the provider; omit it to leave the whole provider
	// configuration untouched.
	Config json.RawMessage `json:"config"`
}

type apiConfigTestRequest struct {
	Kind         string          `json:"kind"`
	ExistingName string          `json:"existing_name,omitempty"`
	Config       json.RawMessage `json:"config"`
	SpecPatch    json.RawMessage `json:"spec_patch,omitempty"`
}

type apiGroupRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (r apiServiceRequest) service() (*apicatalog.Service, error) {
	specs := make([]string, 0, len(r.Specs))
	for _, s := range r.Specs {
		if s = strings.TrimSpace(s); s != "" {
			specs = append(specs, s)
		}
	}

	svc := &apicatalog.Service{
		Name:        strings.TrimSpace(strings.ToLower(r.Name)),
		Kind:        strings.TrimSpace(strings.ToLower(r.Kind)),
		Group:       r.Group,
		Description: strings.TrimSpace(r.Description),
		BaseURL:     strings.TrimSpace(r.BaseURL),
		Config:      r.Config,
		Specs:       specs,
	}

	if r.RefreshInterval != "" {
		d, err := time.ParseDuration(r.RefreshInterval)
		if err != nil {
			return nil, err
		}
		svc.RefreshInterval = d
	}

	// Route the override fields through the update validator so a create cannot
	// store a base URL or a spec patch that an update would have rejected.
	envelope := map[string]any{"base_url": svc.BaseURL, "group": svc.Group}
	if len(r.SpecPatch) > 0 {
		envelope["spec_patch"] = r.SpecPatch
	}
	if len(r.Operations) > 0 {
		envelope["operations"] = r.Operations
	}

	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}

	var update apicatalog.ServiceUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		return nil, err
	}
	if _, err := update.Apply(svc); err != nil {
		return nil, err
	}

	return svc, nil
}

// apiServiceView is the REST shape of a service: secrets are reduced to
// set/unset booleans by the provider, the refresh interval is a duration string
// and the scope key plus live activity are included for the UI.
type apiServiceView struct {
	*apicatalog.Service

	RefreshInterval string             `json:"refresh_interval"`
	Config          any                `json:"config,omitempty"`
	ScopeKey        string             `json:"scope_key"`
	EffectiveGroup  string             `json:"effective_group"`
	Running         string             `json:"running,omitempty"`
	TaskState       string             `json:"task_state,omitempty"`
	Progress        []manager.Progress `json:"progress,omitempty"`
}

func viewAPIService(mgr *manager.Manager, svc *apicatalog.Service) apiServiceView {
	interval := ""
	if svc.RefreshInterval > 0 {
		interval = svc.RefreshInterval.String()
	}

	scope := apicatalog.ScopeKey(svc.Name)
	progress, _ := mgr.Progress(scope)

	return apiServiceView{
		Service:         svc,
		RefreshInterval: interval,
		Config:          mgr.APIServiceConfigView(svc),
		ScopeKey:        scope,
		EffectiveGroup:  svc.EffectiveGroup(),
		Running:         mgr.Activity(scope),
		TaskState:       mgr.TaskState(scope),
		Progress:        progress,
	}
}

func listAPIGroups(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		groups, err := mgr.APIGroups(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(groups)
	}
}

func upsertAPIGroup(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req apiGroupRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		// A navigating client must not be able to abort a write half-way.
		group, err := mgr.UpsertAPIGroup(context.WithoutCancel(c.Request.Context()), req.Name, req.Description)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(group)
	}
}

func deleteAPIGroup(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")
		if err := mgr.DeleteAPIGroup(context.WithoutCancel(c.Request.Context()), name); err != nil {
			return c.Err(err)
		}

		return c.SendJSON(map[string]string{"status": "deleted"})
	}
}

// listAPIServices returns one page of services, filtered by ?group and ?search.
func listAPIServices(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		query := c.Request.URL.Query()

		page := max(queryInt(query.Get("page"), 1), 1)
		perPage := queryInt(query.Get("per_page"), 100)
		if perPage < 1 {
			perPage = 100
		}
		if perPage > 200 {
			perPage = 200
		}

		services, total, err := mgr.APIServicesPaged(c.Request.Context(),
			query.Get("group"), query.Get("search"), (page-1)*perPage, perPage)
		if err != nil {
			return c.Err(err)
		}

		views := make([]apiServiceView, 0, len(services))
		for _, svc := range services {
			views = append(views, viewAPIService(mgr, svc))
		}

		return c.SendJSON(map[string]any{
			"services": views,
			"total":    total,
			"page":     page,
			"per_page": perPage,
			"has_more": page*perPage < total,
		})
	}
}

func addAPIService(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req apiServiceRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		svc, err := req.service()
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		if err := mgr.AddAPIService(context.WithoutCancel(c.Request.Context()), svc); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(viewAPIService(mgr, svc))
	}
}

// getAPIService returns a service plus one page of its endpoints, filtered by
// ?q, ?tag and ?method. Endpoints are paged at the store level so a large
// specification is never loaded whole.
func getAPIService(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")

		svc, err := mgr.APIService(c.Request.Context(), name)
		if err != nil {
			return c.Err(err)
		}
		if svc == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		query := c.Request.URL.Query()

		page := max(queryInt(query.Get("page"), 1), 1)
		perPage := queryInt(query.Get("per_page"), 50)
		if perPage < 1 {
			perPage = 50
		}
		if perPage > 200 {
			perPage = 200
		}

		ops, total, err := mgr.APIOperationsPaged(c.Request.Context(), name,
			query.Get("q"), query.Get("tag"), query.Get("method"), (page-1)*perPage, perPage)
		if err != nil {
			return c.Err(err)
		}

		tags, err := mgr.APITags(c.Request.Context(), name)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(map[string]any{
			"service":    viewAPIService(mgr, svc),
			"operations": ops,
			"tags":       tags,
			"total":      total,
			"page":       page,
			"per_page":   perPage,
			"has_more":   page*perPage < total,
		})
	}
}

// getAPIOperation returns one endpoint's full structured detail.
func getAPIOperation(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")
		handle := c.Request.URL.Query().Get("id")

		op, err := mgr.APIOperation(c.Request.Context(), name, handle)
		if err != nil {
			return c.Err(err)
		}
		if op == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		var detail any
		if len(op.Detail) > 0 {
			// Sent through as raw JSON: the stored blob is already the exact
			// shape the client wants, and decoding it here only to re-encode it
			// would cost a round trip through a recursive schema for nothing.
			detail = json.RawMessage(op.Detail)
		}

		return c.SendJSON(map[string]any{"operation": op, "detail": detail})
	}
}

func updateAPIService(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req apiServiceUpdateRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		name := c.Request.PathValue("name")

		if err := mgr.UpdateAPIService(context.WithoutCancel(c.Request.Context()), name, req.ServiceUpdate, req.Config); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		svc, err := mgr.APIService(c.Request.Context(), name)
		if err != nil {
			return c.Err(err)
		}
		if svc == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		return c.SendJSON(viewAPIService(mgr, svc))
	}
}

func deleteAPIService(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")

		if err := mgr.DeleteAPIService(context.WithoutCancel(c.Request.Context()), name); err != nil {
			return c.Err(err)
		}

		return c.SendJSON(map[string]string{"status": "deleted"})
	}
}

func refreshAPIService(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")

		svc, err := mgr.APIService(c.Request.Context(), name)
		if err != nil {
			return c.Err(err)
		}
		if svc == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		// force=true re-renders from the same document, for when an override
		// changed but the upstream specification did not.
		if c.Request.URL.Query().Get("force") == "true" {
			mgr.TriggerAPIFullRefresh(name)
		} else {
			mgr.TriggerAPIRefresh(name)
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(map[string]string{"status": "queued"})
	}
}

func cancelAPIService(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")

		cancelled := mgr.CancelTasks(apicatalog.ScopeKey(name))

		return c.SendJSON(map[string]any{"status": "cancelled", "tasks": cancelled})
	}
}

func testAPIServiceConfig(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req apiConfigTestRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		result, err := mgr.TestAPIServiceConfig(c.Request.Context(), req.ExistingName, req.Kind, req.Config, req.SpecPatch)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(result)
	}
}

func listAPIServiceKinds(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		return c.SendJSON(map[string]any{"kinds": mgr.APIServiceKinds()})
	}
}
