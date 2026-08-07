package openapi

import (
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// walkV3 emits one operation per path/method of an OpenAPI 3.x document.
func walkV3(svc *apicatalog.Service, doc *v3.Document, emit apicatalog.Emit) (apicatalog.ServiceInfo, error) {
	info := apicatalog.ServiceInfo{}
	if doc.Info != nil {
		info.Title = trimText(doc.Info.Title)
		info.Version = trimText(doc.Info.Version)
		info.Summary = trimText(doc.Info.Description)
	}

	var specURL string
	if len(doc.Servers) > 0 {
		specURL = doc.Servers[0].URL
	}
	baseURL := resolveBaseURL(svc.BaseURL, specURL)
	info.ResolvedURL = baseURL

	schemes := v3SecuritySchemes(doc)
	globalSecurity := v3Security(doc.Security, schemes)

	if doc.Paths == nil || doc.Paths.PathItems == nil {
		return info, nil
	}

	for path, item := range doc.Paths.PathItems.FromOldest() {
		if item == nil {
			continue
		}

		for method, op := range v3Operations(item) {
			if op == nil {
				continue
			}

			d, truncated := v3Detail(svc, doc, item, op, method, path, baseURL, schemes, globalSecurity)
			if err := emitOperation(svc, d, op.OperationId, truncated, emit); err != nil {
				return info, err
			}
		}
	}

	return info, nil
}

// v3Operations returns the operations declared on a path item, in a fixed
// order.
//
// The order is fixed rather than taken from the document because two servers
// serving the same API can emit the map in different orders, and the catalog's
// listing would then reshuffle on every refresh for no reason.
func v3Operations(item *v3.PathItem) map[string]*v3.Operation {
	ops := map[string]*v3.Operation{}

	for method, op := range map[string]*v3.Operation{
		"GET":     item.Get,
		"PUT":     item.Put,
		"POST":    item.Post,
		"DELETE":  item.Delete,
		"OPTIONS": item.Options,
		"HEAD":    item.Head,
		"PATCH":   item.Patch,
		"TRACE":   item.Trace,
	} {
		if op != nil {
			ops[method] = op
		}
	}

	if item.AdditionalOperations != nil {
		for method, op := range item.AdditionalOperations.FromOldest() {
			if op != nil {
				ops[strings.ToUpper(method)] = op
			}
		}
	}

	return ops
}

func v3Detail(
	svc *apicatalog.Service,
	doc *v3.Document,
	item *v3.PathItem,
	op *v3.Operation,
	method, path, baseURL string,
	schemes map[string]*v3.SecurityScheme,
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
	}
	if op.Deprecated != nil {
		d.Deprecated = *op.Deprecated
	}
	if d.Description == "" {
		d.Description = trimText(item.Description)
	}

	// Path-level parameters apply to every operation on the path; operation
	// parameters override them by (name, in).
	d.Parameters = v3Params(f, item.Parameters, op.Parameters)

	d.RequestBody = v3RequestBody(f, op.RequestBody)
	d.Responses = v3Responses(f, op.Responses)

	if op.Security != nil {
		d.Security = v3Security(op.Security, schemes)
	} else {
		d.Security = globalSecurity
	}

	// A server declared on the operation or the path overrides the document's,
	// but never the operator's: an explicit BaseURL is the whole point of the
	// override and must win over anything the document says.
	if svc.BaseURL == "" {
		if len(op.Servers) > 0 {
			d.BaseURL = resolveBaseURL("", op.Servers[0].URL)
		} else if len(item.Servers) > 0 {
			d.BaseURL = resolveBaseURL("", item.Servers[0].URL)
		}
	}
	_ = doc

	return d, f.truncated
}

// v3Params merges path-level and operation-level parameters, with the
// operation's winning on a (name, in) collision.
func v3Params(f *flattener, inherited, own []*v3.Parameter) []apicatalog.Param {
	type key struct{ name, in string }

	seen := map[key]int{}
	out := make([]apicatalog.Param, 0, len(inherited)+len(own))

	add := func(p *v3.Parameter) {
		if p == nil || p.Name == "" {
			return
		}

		param := apicatalog.Param{
			Name:        p.Name,
			In:          p.In,
			Description: trimText(p.Description),
			Example:     nodeString(p.Example),
		}
		if p.Required != nil {
			param.Required = *p.Required
		}
		// A path parameter is required by definition; specs frequently omit
		// the flag, and a recipe that leaves it out produces a URL with a
		// literal "{id}" in it.
		if strings.EqualFold(p.In, "path") {
			param.Required = true
		}

		if p.Schema != nil {
			if s := f.schema(p.Schema, apicatalog.MaxSchemaDepth); s != nil {
				param.Type = s.Type
				param.Enum = s.Enum
				if param.Description == "" {
					param.Description = s.Description
				}
			}
			if resolved := p.Schema.Schema(); resolved != nil {
				param.Default = nodeString(resolved.Default)
				if param.Example == "" {
					param.Example = nodeString(resolved.Example)
				}
			}
		}

		k := key{name: p.Name, in: p.In}
		if idx, ok := seen[k]; ok {
			out[idx] = param

			return
		}
		seen[k] = len(out)
		out = append(out, param)
	}

	for _, p := range inherited {
		add(p)
	}
	for _, p := range own {
		add(p)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func v3RequestBody(f *flattener, body *v3.RequestBody) *apicatalog.Body {
	if body == nil || body.Content == nil {
		return nil
	}

	contentTypes := make([]string, 0, 4)
	media := map[string]*v3.MediaType{}
	for ct, mt := range body.Content.FromOldest() {
		contentTypes = append(contentTypes, ct)
		media[ct] = mt
	}

	chosen := firstContent(contentTypes)
	if chosen == "" {
		return nil
	}

	out := &apicatalog.Body{
		ContentType: chosen,
		Description: trimText(body.Description),
	}
	if body.Required != nil {
		out.Required = *body.Required
	}
	if mt := media[chosen]; mt != nil {
		out.Schema = f.schema(mt.Schema, 0)
	}

	return out
}

// maxResponses bounds how many status codes are documented. A catalog entry
// exists to help build a request; enumerating every error code a gateway can
// produce crowds out the part that matters.
const maxResponses = 6

func v3Responses(f *flattener, responses *v3.Responses) []apicatalog.Response {
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

		if resp.Content != nil {
			contentTypes := make([]string, 0, 4)
			media := map[string]*v3.MediaType{}
			for ct, mt := range resp.Content.FromOldest() {
				contentTypes = append(contentTypes, ct)
				media[ct] = mt
			}
			if chosen := firstContent(contentTypes); chosen != "" {
				item.ContentType = chosen
				if mt := media[chosen]; mt != nil {
					item.Schema = f.schema(mt.Schema, 0)
				}
			}
		}

		out = append(out, item)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func v3SecuritySchemes(doc *v3.Document) map[string]*v3.SecurityScheme {
	out := map[string]*v3.SecurityScheme{}
	if doc.Components == nil || doc.Components.SecuritySchemes == nil {
		return out
	}

	for name, scheme := range doc.Components.SecuritySchemes.FromOldest() {
		out[name] = scheme
	}

	return out
}

func v3Security(requirements []*base.SecurityRequirement, schemes map[string]*v3.SecurityScheme) []apicatalog.SecurityScheme {
	var out []apicatalog.SecurityScheme

	for _, req := range requirements {
		if req == nil || req.Requirements == nil {
			continue
		}

		for name, scopes := range req.Requirements.FromOldest() {
			entry := apicatalog.SecurityScheme{Name: name, Scopes: scopes}
			if scheme := schemes[name]; scheme != nil {
				entry.Type = scheme.Type
				entry.Scheme = scheme.Scheme
				entry.In = scheme.In
				entry.ParamName = scheme.Name
				entry.Description = trimText(scheme.Description)
			}
			out = append(out, entry)
		}
	}

	return out
}
