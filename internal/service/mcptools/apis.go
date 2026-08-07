package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/manager"
)

// The API-catalog tools implement one deliberate shape: progressive disclosure.
//
// A single OpenAPI document routinely runs to hundreds of kilobytes. Handing
// one to a model is both unaffordable and useless — the answer to "how do I
// create an invoice" is thirty lines buried in fifty thousand. So the catalog is
// read in four widening steps, each of which costs about what it is worth:
//
//	list_api_groups     which area of the estate      (tens of tokens)
//	list_api_services   which API in that area        (tens per service)
//	list_api_endpoints  which endpoint in that API    (~20 per endpoint)
//	get_api_endpoint    exactly how to call it        (bounded, ~1-3k)
//
// The alternative — one tool returning everything — is why "just give the model
// the swagger file" does not work in practice.

// ---- arguments -------------------------------------------------------------

type listAPIServicesArgs struct {
	Group   string `json:"group,omitempty"    jsonschema:"restrict to one group; omit to list every group's services"`
	Search  string `json:"search,omitempty"   jsonschema:"case-insensitive substring filter on the service name"`
	Page    int    `json:"page,omitempty"     jsonschema:"page number (default 1)"`
	PerPage int    `json:"per_page,omitempty" jsonschema:"results per page (default 50, max 200)"`
}

type listAPIEndpointsArgs struct {
	Service string `json:"service"           jsonschema:"the api service name from list_api_services"`
	Search  string `json:"search,omitempty"  jsonschema:"case-insensitive substring matched against the path, summary and operation id"`
	Tag     string `json:"tag,omitempty"     jsonschema:"restrict to one tag (an OpenAPI tag, or the gRPC service name)"`
	Method  string `json:"method,omitempty"  jsonschema:"restrict to one HTTP method (GET, POST, ...)"`
	Page    int    `json:"page,omitempty"     jsonschema:"page number (default 1)"`
	PerPage int    `json:"per_page,omitempty" jsonschema:"results per page (default 50, max 200)"`
}

type getAPIEndpointArgs struct {
	Service  string `json:"service"  jsonschema:"the api service name"`
	Endpoint string `json:"endpoint" jsonschema:"the operation id from list_api_endpoints; 'METHOD /path' also resolves"`
}

type apiServiceNameArgs struct {
	Name string `json:"name" jsonschema:"the api service name"`
}

type apiGroupArgs struct {
	Name        string `json:"name"                  jsonschema:"the group name (lowercase [a-z0-9._-])"`
	Description string `json:"description,omitempty" jsonschema:"what this group holds; this is what a model reads to pick a group, so describe the domain"`
}

type apiGroupNameArgs struct {
	Name string `json:"name" jsonschema:"the group name"`
}

type addAPIServiceArgs struct {
	Name        string `json:"name"                  jsonschema:"unique service name (lowercase [a-z0-9._-]); becomes the search scope key api:<name>"`
	Kind        string `json:"kind"                  jsonschema:"provider kind from api_service_kinds (e.g. openapi)"`
	Group       string `json:"group,omitempty"       jsonschema:"group to file the service under; omit for the default group"`
	Description string `json:"description,omitempty" jsonschema:"human summary; overrides the specification's own description"`
	BaseURL     string `json:"base_url,omitempty"    jsonschema:"override the servers the document declares, e.g. the internal deployment URL"`

	Config json.RawMessage `json:"config,omitempty" jsonschema:"provider config; for openapi set url (where the document is served) and optionally user/token/headers"`

	SpecPatch json.RawMessage `json:"spec_patch,omitempty" jsonschema:"RFC 7386 JSON Merge Patch applied to the raw document before parsing; use it to correct schemas or metadata, null deletes a key"`

	Operations map[string]apicatalog.OperationOverride `json:"operations,omitempty" jsonschema:"per-operation overrides keyed by operation id or 'METHOD /path'; set hidden to drop an endpoint from the catalog"`

	RefreshInterval string   `json:"refresh_interval,omitempty" jsonschema:"Go duration between automatic syncs (e.g. 24h); omit for manual only"`
	Specs           []string `json:"specs,omitempty"            jsonschema:"cron schedules for automatic syncs; when set they replace refresh_interval"`
}

type updateAPIServiceArgs struct {
	Name string `json:"name" jsonschema:"the api service name"`

	Group       *string `json:"group,omitempty"`
	Description *string `json:"description,omitempty"`
	BaseURL     *string `json:"base_url,omitempty"`

	Config    json.RawMessage `json:"config,omitempty"     jsonschema:"provider config changes; blank write-only secrets keep the stored value"`
	SpecPatch json.RawMessage `json:"spec_patch,omitempty" jsonschema:"replace the JSON Merge Patch applied to the raw document"`

	Operations map[string]apicatalog.OperationOverride `json:"operations,omitempty" jsonschema:"replace the per-operation override map wholesale"`

	RefreshInterval *string  `json:"refresh_interval,omitempty"`
	Specs           []string `json:"specs,omitempty"`
}

// ---- outputs ---------------------------------------------------------------

type apiGroupListOutput struct {
	Groups []apicatalog.GroupSummary `json:"groups"`
}

// apiServiceSummary is the listing shape: enough to choose a service, nothing
// more. Config, overrides and state are deliberately absent.
type apiServiceSummary struct {
	Name           string `json:"name"`
	Group          string `json:"group"`
	Kind           string `json:"kind"`
	ScopeKey       string `json:"scope_key"`
	Title          string `json:"title,omitempty"`
	Version        string `json:"version,omitempty"`
	Description    string `json:"description,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	OperationCount int    `json:"operation_count"`
	Status         string `json:"status"`
	LastError      string `json:"last_error,omitempty"`
}

// apiEndpointSummary is one line of a listing. It is kept to the fields a model
// needs to pick an endpoint, because this is the shape that gets multiplied by
// a page of fifty.
type apiEndpointSummary struct {
	OperationID string   `json:"operation_id"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Summary     string   `json:"summary,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`
}

type apiEndpointListOutput struct {
	Service   string               `json:"service"`
	Endpoints []apiEndpointSummary `json:"endpoints"`
	Total     int                  `json:"total"`
	Page      int                  `json:"page"`
	PerPage   int                  `json:"per_page"`
	HasMore   bool                 `json:"has_more"`
	// Tags lists the service's tag vocabulary so a follow-up call can narrow
	// by tag without a second discovery round trip.
	Tags []string `json:"tags,omitempty"`
}

// apiEndpointOutput is returned as JSON content without a declared output
// schema.
//
// apicatalog.Schema is recursive (an object's property is a schema), and a
// recursive Go type cannot be reflected into a flat JSON Schema — the generator
// detects the cycle and refuses. Expressing it would need $ref, which is the
// same construct sanitizeSchema exists to keep out of tool schemas because
// Gemini's function calling rejects it. A declared schema here would therefore
// have to be either a lie or unusable, so the tool returns plain JSON like
// search_code and get_doc do.
type apiEndpointOutput struct {
	Service     string             `json:"service"`
	OperationID string             `json:"operation_id"`
	ScopeKey    string             `json:"scope_key"`
	DocPath     string             `json:"doc_path"`
	Detail      *apicatalog.Detail `json:"detail"`
}

type apiServiceConfigOutput struct {
	apiServiceSummary

	SpecPatch  json.RawMessage                         `json:"spec_patch,omitempty"`
	Operations map[string]apicatalog.OperationOverride `json:"operations,omitempty"`
	Specs      []string                                `json:"specs,omitempty"`
	Config     any                                     `json:"config,omitempty"`
	Running    string                                  `json:"running,omitempty"`
}

// ---- registration ----------------------------------------------------------

// addAPITools registers the API-catalog discovery tools, plus the
// administration tools when the full profile is in use.
func addAPITools(server *mcp.Server, mgr *manager.Manager, includeAdmin bool) {
	addTool(server, &mcp.Tool{
		Name: "list_api_groups",
		Description: "List the API catalog's groups with their descriptions and service counts. " +
			"Start here when a question is about calling an API and the right service is unknown: the group " +
			"description says which part of the estate it covers. Then call list_api_services with the group.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, apiGroupListOutput, error) {
		groups, err := mgr.APIGroups(ctx)
		if err != nil {
			return nil, apiGroupListOutput{}, err
		}

		out := apiGroupListOutput{Groups: groups}

		return jsonResult(out), out, nil
	})

	addTool(server, &mcp.Tool{
		Name: "list_api_services",
		Description: "List catalogued API services with their title, description, base URL and endpoint count. " +
			"Each service is one OpenAPI document or gRPC server. Narrow with group or search rather than " +
			"paging through everything, then call list_api_endpoints with the service name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listAPIServicesArgs) (*mcp.CallToolResult, pageResult[apiServiceSummary], error) {
		var zero pageResult[apiServiceSummary]

		page := max(args.Page, 1)
		perPage := boundedCount(args.PerPage, 50, 200)

		services, total, err := mgr.APIServicesPaged(ctx, args.Group, args.Search, (page-1)*perPage, perPage)
		if err != nil {
			return nil, zero, err
		}

		items := make([]apiServiceSummary, 0, len(services))
		for _, svc := range services {
			items = append(items, summarizeAPIService(svc))
		}

		out := pageResult[apiServiceSummary]{
			Items: items, Total: total, Page: page,
			PerPage: perPage, HasMore: page*perPage < total,
		}

		return jsonResult(out), out, nil
	})

	addTool(server, &mcp.Tool{
		Name: "list_api_endpoints",
		Description: "List one service's endpoints as compact one-line summaries (method, path, summary, tags). " +
			"Filter with search/tag/method to find the operation that does what you need, then call " +
			"get_api_endpoint for its parameters, request schema and a ready-to-run request.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listAPIEndpointsArgs) (*mcp.CallToolResult, apiEndpointListOutput, error) {
		var zero apiEndpointListOutput

		service := strings.TrimSpace(strings.ToLower(args.Service))
		if service == "" {
			return nil, zero, fmt.Errorf("service is required")
		}

		page := max(args.Page, 1)
		perPage := boundedCount(args.PerPage, 50, 200)

		ops, total, err := mgr.APIOperationsPaged(ctx, service, args.Search, args.Tag, args.Method, (page-1)*perPage, perPage)
		if err != nil {
			return nil, zero, err
		}

		items := make([]apiEndpointSummary, 0, len(ops))
		for _, op := range ops {
			items = append(items, apiEndpointSummary{
				OperationID: op.OperationID,
				Method:      op.Method,
				Path:        op.Path,
				Summary:     op.Summary,
				Tags:        op.Tags,
				Deprecated:  op.Deprecated,
			})
		}

		out := apiEndpointListOutput{
			Service: service, Endpoints: items, Total: total,
			Page: page, PerPage: perPage, HasMore: page*perPage < total,
		}

		// Only offer the tag vocabulary when the result did not already fit in
		// one page: with everything visible there is nothing left to narrow.
		if out.HasMore {
			if tags, err := mgr.APITags(ctx, service); err == nil {
				out.Tags = tags
			}
		}

		return jsonResult(out), out, nil
	})

	addTool(server, &mcp.Tool{
		Name: "get_api_endpoint",
		Description: "Get everything needed to call one endpoint: parameters, request and response schemas, " +
			"authentication, and a ready-to-run curl command with an example body. Schemas are flattened and " +
			"depth-limited; when truncated is set, consult the source specification for the full definition.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args getAPIEndpointArgs) (*mcp.CallToolResult, any, error) {
		service := strings.TrimSpace(strings.ToLower(args.Service))
		if service == "" {
			return nil, nil, fmt.Errorf("service is required")
		}

		op, err := mgr.APIOperation(ctx, service, args.Endpoint)
		if err != nil {
			return nil, nil, err
		}
		if op == nil {
			return nil, nil, fmt.Errorf("endpoint %q not found in api service %s; call list_api_endpoints to see what it has",
				args.Endpoint, service)
		}

		out := apiEndpointOutput{
			Service:     service,
			OperationID: op.OperationID,
			ScopeKey:    apicatalog.ScopeKey(service),
			DocPath:     op.OpSlug + ".md",
		}
		if len(op.Detail) > 0 {
			var detail apicatalog.Detail
			if err := json.Unmarshal(op.Detail, &detail); err != nil {
				return nil, nil, fmt.Errorf("decode endpoint detail; %w", err)
			}
			out.Detail = &detail
		}

		return jsonResult(out), nil, nil
	})

	if includeAdmin {
		addAPIAdminTools(server, mgr)
	}
}

// addAPIAdminTools registers the catalog management tools. Admin profile only:
// they point krabby at arbitrary URLs and write persistent state.
func addAPIAdminTools(server *mcp.Server, mgr *manager.Manager) {
	addTool(server, &mcp.Tool{
		Name:        "api_service_kinds",
		Description: "List the API service kinds that can be created (e.g. openapi) so add_api_service is called with a valid kind.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		return jsonResult(map[string]any{"kinds": mgr.APIServiceKinds()}), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "set_api_group_description",
		Description: "Create or describe an API group. Groups are created implicitly by naming one on a service; " +
			"this only sets the description a model reads when choosing where to look.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args apiGroupArgs) (*mcp.CallToolResult, any, error) {
		group, err := mgr.UpsertAPIGroup(ctx, args.Name, args.Description)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(group), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "delete_api_group",
		Description: "Delete an API group's description. Services keep their group tag, so the group becomes description-less rather than empty.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args apiGroupNameArgs) (*mcp.CallToolResult, any, error) {
		if err := mgr.DeleteAPIGroup(ctx, args.Name); err != nil {
			return nil, nil, err
		}

		return textResult("deleted"), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "add_api_service",
		Description: "Catalogue an API from a served OpenAPI/Swagger document and index its endpoints under scope api:<name>, " +
			"then trigger the first background sync. Use base_url when the document names a host the caller cannot reach, " +
			"spec_patch to correct the document itself, and operations to fix or hide individual endpoints. " +
			"Poll list_api_services until status is ready.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args addAPIServiceArgs) (*mcp.CallToolResult, any, error) {
		svc := &apicatalog.Service{
			Name:        strings.TrimSpace(strings.ToLower(args.Name)),
			Kind:        strings.TrimSpace(strings.ToLower(args.Kind)),
			Group:       args.Group,
			Description: strings.TrimSpace(args.Description),
			BaseURL:     strings.TrimRight(strings.TrimSpace(args.BaseURL), "/"),
			Config:      args.Config,
			Specs:       args.Specs,
		}

		// Reuse the update path's validation and normalization for the fields
		// both surfaces accept, so a create cannot store something an update
		// would have rejected.
		update := apicatalog.ServiceUpdate{}
		if err := json.Unmarshal(buildAPICreateUpdate(args), &update); err != nil {
			return nil, nil, err
		}
		if _, err := update.Apply(svc); err != nil {
			return nil, nil, err
		}

		if err := mgr.AddAPIService(ctx, svc); err != nil {
			return nil, nil, err
		}

		return jsonResult(map[string]any{
			"name": svc.Name, "kind": svc.Kind,
			"scope_key": apicatalog.ScopeKey(svc.Name), "status": svc.Status,
		}), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "update_api_service",
		Description: "Change a catalogued API service: its group, description, base-URL override, spec patch, " +
			"per-operation overrides, schedule or provider config. Only the properties present in the call change; " +
			"an override change re-syncs the service so the new rendering takes effect immediately.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args updateAPIServiceArgs) (*mcp.CallToolResult, any, error) {
		name := strings.TrimSpace(strings.ToLower(args.Name))

		// Typed arguments cannot express "the caller omitted this", which is
		// exactly the distinction the partial-update rule turns on, so the
		// envelope is rebuilt from the raw arguments.
		raw, err := apiUpdateEnvelope(req)
		if err != nil {
			return nil, nil, err
		}

		var update apicatalog.ServiceUpdate
		if err := json.Unmarshal(raw, &update); err != nil {
			return nil, nil, fmt.Errorf("decode update; %w", err)
		}

		if err := mgr.UpdateAPIService(ctx, name, update, args.Config); err != nil {
			return nil, nil, err
		}

		return textResult("updated"), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "delete_api_service",
		Description: "Delete a catalogued API service, its endpoints, its markdown projections and its search index entries.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args apiServiceNameArgs) (*mcp.CallToolResult, any, error) {
		name := strings.TrimSpace(strings.ToLower(args.Name))
		if err := mgr.DeleteAPIService(ctx, name); err != nil {
			return nil, nil, err
		}

		return textResult("deleted"), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "refresh_api_service",
		Description: "Re-fetch a service's document and re-index its endpoints in the background. Returns immediately; poll list_api_services for status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args apiServiceNameArgs) (*mcp.CallToolResult, any, error) {
		name := strings.TrimSpace(strings.ToLower(args.Name))

		svc, err := mgr.APIService(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		if svc == nil {
			return nil, nil, fmt.Errorf("api service %s not found", name)
		}

		mgr.TriggerAPIRefresh(name)

		return textResult("refresh queued"), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "get_api_service_config",
		Description: "Inspect one service's full administrative state: schedule, overrides, spec patch and provider config with secrets redacted.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args apiServiceNameArgs) (*mcp.CallToolResult, apiServiceConfigOutput, error) {
		var zero apiServiceConfigOutput

		name := strings.TrimSpace(strings.ToLower(args.Name))
		svc, err := mgr.APIService(ctx, name)
		if err != nil {
			return nil, zero, err
		}
		if svc == nil {
			return nil, zero, fmt.Errorf("api service %s not found", name)
		}

		out := apiServiceConfigOutput{
			apiServiceSummary: summarizeAPIService(svc),
			SpecPatch:         svc.SpecPatch,
			Operations:        svc.Operations,
			Specs:             svc.Specs,
			Config:            mgr.APIServiceConfigView(svc),
			Running:           mgr.Activity(apicatalog.ScopeKey(name)),
		}

		return jsonResult(out), out, nil
	})
}

// ---- helpers ---------------------------------------------------------------

func summarizeAPIService(svc *apicatalog.Service) apiServiceSummary {
	if svc == nil {
		return apiServiceSummary{}
	}

	baseURL := svc.ResolvedBaseURL
	if svc.BaseURL != "" {
		baseURL = svc.BaseURL
	}

	return apiServiceSummary{
		Name:           svc.Name,
		Group:          svc.EffectiveGroup(),
		Kind:           svc.Kind,
		ScopeKey:       apicatalog.ScopeKey(svc.Name),
		Title:          svc.Title,
		Version:        svc.Version,
		Description:    svc.EffectiveDescription(),
		BaseURL:        baseURL,
		OperationCount: svc.OperationCount,
		Status:         svc.Status,
		LastError:      svc.LastError,
	}
}

// buildAPICreateUpdate renders the create arguments that the update envelope
// also owns, so both paths share one validator.
func buildAPICreateUpdate(args addAPIServiceArgs) []byte {
	envelope := map[string]any{}

	if args.Group != "" {
		envelope["group"] = args.Group
	}
	if len(args.SpecPatch) > 0 {
		envelope["spec_patch"] = args.SpecPatch
	}
	if len(args.Operations) > 0 {
		envelope["operations"] = args.Operations
	}
	if args.RefreshInterval != "" {
		envelope["refresh_interval"] = args.RefreshInterval
	}

	raw, err := json.Marshal(envelope)
	if err != nil {
		return []byte("{}")
	}

	return raw
}

// apiUpdateEnvelope rebuilds the partial-update payload from the raw tool
// arguments, keeping only the properties the caller actually sent.
//
// It exists because the typed argument struct cannot carry the distinction the
// merge rule depends on: a Go zero value cannot say whether the caller wrote
// "base_url": null (clear it) or left the property out (keep it). Reading the
// raw arguments is the only place that information still exists.
func apiUpdateEnvelope(req *mcp.CallToolRequest) ([]byte, error) {
	fields, err := argFields(req.Params.Arguments)
	if err != nil {
		return nil, err
	}

	envelope := map[string]json.RawMessage{}
	for _, key := range []string{"group", "description", "base_url", "spec_patch", "operations", "refresh_interval", "specs"} {
		if value, ok := fields[key]; ok {
			envelope[key] = value
		}
	}

	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode update; %w", err)
	}

	return raw, nil
}
