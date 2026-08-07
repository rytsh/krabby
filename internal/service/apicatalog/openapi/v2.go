package openapi

import (
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	v2 "github.com/pb33f/libopenapi/datamodel/high/v2"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// walkV2 emits one operation per path/method of a Swagger 2.0 document.
//
// Swagger differs from OpenAPI 3 in three places that matter here: the base URL
// is assembled from scheme/host/basePath instead of a servers list, the request
// body is a parameter with in:"body" instead of a separate requestBody, and
// content types are declared once per operation (consumes/produces) instead of
// per schema. Everything past those three is shared with the v3 walker.
func walkV2(svc *apicatalog.Service, doc *v2.Swagger, emit apicatalog.Emit) (apicatalog.ServiceInfo, error) {
	info := apicatalog.ServiceInfo{}
	if doc.Info != nil {
		info.Title = trimText(doc.Info.Title)
		info.Version = trimText(doc.Info.Version)
		info.Summary = trimText(doc.Info.Description)
	}

	baseURL := resolveBaseURL(svc.BaseURL, v2BaseURL(doc))
	info.ResolvedURL = baseURL

	schemes := v2SecuritySchemes(doc)
	globalSecurity := v2Security(doc.Security, schemes)

	if doc.Paths == nil || doc.Paths.PathItems == nil {
		return info, nil
	}

	for path, item := range doc.Paths.PathItems.FromOldest() {
		if item == nil {
			continue
		}

		for method, op := range v2Operations(item) {
			if op == nil {
				continue
			}

			d, truncated := v2Detail(doc, item, op, method, path, baseURL, schemes, globalSecurity)
			if err := emitOperation(svc, d, op.OperationId, truncated, emit); err != nil {
				return info, err
			}
		}
	}

	return info, nil
}

// v2BaseURL assembles the document's base URL from scheme, host and basePath.
//
// A Swagger document is allowed to omit all three, meaning "same host as this
// document" — which krabby cannot know, so the result is a bare basePath and
// the recipe comes out relative. That is honest: an operator who needs an
// absolute URL sets the service's BaseURL override, which is exactly what the
// override exists for.
func v2BaseURL(doc *v2.Swagger) string {
	scheme := "https"
	for _, s := range doc.Schemes {
		if strings.EqualFold(s, "https") {
			scheme = "https"

			break
		}
		if strings.EqualFold(s, "http") {
			scheme = "http"
		}
	}

	host := strings.TrimSpace(doc.Host)
	base := strings.TrimSpace(doc.BasePath)

	if host == "" {
		return base
	}

	return scheme + "://" + host + base
}

// v2Operations returns the operations declared on a path item.
func v2Operations(item *v2.PathItem) map[string]*v2.Operation {
	ops := map[string]*v2.Operation{}

	for method, op := range map[string]*v2.Operation{
		"GET":     item.Get,
		"PUT":     item.Put,
		"POST":    item.Post,
		"DELETE":  item.Delete,
		"OPTIONS": item.Options,
		"HEAD":    item.Head,
		"PATCH":   item.Patch,
	} {
		if op != nil {
			ops[method] = op
		}
	}

	return ops
}

func v2Detail(
	doc *v2.Swagger,
	item *v2.PathItem,
	op *v2.Operation,
	method, path, baseURL string,
	schemes map[string]*v2.SecurityScheme,
	globalSecurity []apicatalog.SecurityScheme,
) (*apicatalog.Detail, bool) {
	f := newFlattener()

	d := &apicatalog.Detail{
		Method:      method,
		Path:        path,
		BaseURL:     baseURL,
		Summary:     trimText(op.Summary),
		Description: trimText(op.Description),
		Tags:        op.Tags,
		Deprecated:  op.Deprecated,
	}

	consumes := op.Consumes
	if len(consumes) == 0 {
		consumes = doc.Consumes
	}
	produces := op.Produces
	if len(produces) == 0 {
		produces = doc.Produces
	}

	d.Parameters, d.RequestBody = v2Params(f, item.Parameters, op.Parameters, consumes)
	d.Responses = v2Responses(f, op.Responses, firstContent(produces))

	if len(op.Security) > 0 {
		d.Security = v2Security(op.Security, schemes)
	} else {
		d.Security = globalSecurity
	}

	return d, f.truncated
}

// v2Params splits Swagger parameters into the catalog's parameter list and its
// request body, merging path-level parameters with the operation's.
func v2Params(f *flattener, inherited, own []*v2.Parameter, consumes []string) ([]apicatalog.Param, *apicatalog.Body) {
	type key struct{ name, in string }

	seen := map[key]int{}
	params := make([]apicatalog.Param, 0, len(inherited)+len(own))
	var body *apicatalog.Body

	add := func(p *v2.Parameter) {
		if p == nil || p.Name == "" {
			return
		}

		// in:"body" is Swagger's request body: a single parameter whose schema
		// is the whole document being sent.
		if strings.EqualFold(p.In, "body") {
			out := &apicatalog.Body{
				ContentType: firstContent(consumes),
				Description: trimText(p.Description),
				Schema:      f.schema(p.Schema, 0),
			}
			if out.ContentType == "" {
				out.ContentType = "application/json"
			}
			if p.Required != nil {
				out.Required = *p.Required
			}
			body = out

			return
		}

		param := apicatalog.Param{
			Name:        p.Name,
			In:          p.In,
			Type:        p.Type,
			Description: trimText(p.Description),
			Enum:        nodeStrings(p.Enum),
			Default:     nodeString(p.Default),
		}
		if p.Required != nil {
			param.Required = *p.Required
		}
		if strings.EqualFold(p.In, "path") {
			param.Required = true
		}
		if param.Type == "array" && p.Items != nil && p.Items.Type != "" {
			param.Type = "array of " + p.Items.Type
		}

		k := key{name: p.Name, in: p.In}
		if idx, ok := seen[k]; ok {
			params[idx] = param

			return
		}
		seen[k] = len(params)
		params = append(params, param)
	}

	for _, p := range inherited {
		add(p)
	}
	for _, p := range own {
		add(p)
	}

	if len(params) == 0 {
		params = nil
	}

	return params, body
}

func v2Responses(f *flattener, responses *v2.Responses, contentType string) []apicatalog.Response {
	if responses == nil || responses.Codes == nil {
		return nil
	}

	out := make([]apicatalog.Response, 0, maxResponses)
	for status, resp := range responses.Codes.FromOldest() {
		if len(out) >= maxResponses {
			break
		}
		if resp == nil {
			continue
		}

		item := apicatalog.Response{
			Status:      status,
			Description: trimText(resp.Description),
		}
		if resp.Schema != nil {
			item.ContentType = contentType
			item.Schema = f.schema(resp.Schema, 0)
		}

		out = append(out, item)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func v2SecuritySchemes(doc *v2.Swagger) map[string]*v2.SecurityScheme {
	out := map[string]*v2.SecurityScheme{}
	if doc.SecurityDefinitions == nil || doc.SecurityDefinitions.Definitions == nil {
		return out
	}

	for name, scheme := range doc.SecurityDefinitions.Definitions.FromOldest() {
		out[name] = scheme
	}

	return out
}

// v2Security maps Swagger security definitions onto the catalog's scheme shape.
//
// Swagger's "basic" type has no OpenAPI 3 equivalent as a type; it is
// http+basic there. Normalizing it here rather than in the renderer means the
// request recipe only has to understand one vocabulary.
func v2Security(requirements []*base.SecurityRequirement, schemes map[string]*v2.SecurityScheme) []apicatalog.SecurityScheme {
	var out []apicatalog.SecurityScheme

	for _, req := range requirements {
		if req == nil || req.Requirements == nil {
			continue
		}

		for name, scopes := range req.Requirements.FromOldest() {
			entry := apicatalog.SecurityScheme{Name: name, Scopes: scopes}

			if scheme := schemes[name]; scheme != nil {
				entry.Description = trimText(scheme.Description)

				switch strings.ToLower(scheme.Type) {
				case "basic":
					entry.Type = "http"
					entry.Scheme = "basic"
				case "apikey":
					entry.Type = "apiKey"
					entry.In = scheme.In
					entry.ParamName = scheme.Name
				case "oauth2":
					entry.Type = "oauth2"
				default:
					entry.Type = scheme.Type
				}
			}

			out = append(out, entry)
		}
	}

	return out
}
