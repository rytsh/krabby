package openapi

import (
	"strings"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// emitOperation applies the service's per-operation overrides, builds the
// request recipe, renders the markdown projection and hands the result to emit.
//
// It is the single place both version walkers converge on, which is what keeps
// a Swagger 2.0 endpoint and an OpenAPI 3.1 endpoint indistinguishable once
// they are in the catalog.
func emitOperation(svc *apicatalog.Service, d *apicatalog.Detail, operationID string, truncated bool, emit apicatalog.Emit) error {
	if truncated {
		d.Truncated = true
	}

	fallbackID := d.Method + " " + d.Path
	if operationID == "" {
		operationID = fallbackID
	}

	// An override may be keyed by operationId or by "METHOD /path". Both are
	// accepted because operationId is optional and unstable: a spec that drops
	// or renames one would otherwise silently detach every override attached
	// to it, and the failure would look like the override never existed.
	override, ok := svc.Override(operationID)
	if !ok {
		override, ok = svc.Override(fallbackID)
	}

	if ok {
		if override.Hidden {
			return nil
		}
		if override.Summary != "" {
			d.Summary = override.Summary
		}
		if override.Description != "" {
			d.Description = override.Description
		}
		if len(override.Tags) > 0 {
			d.Tags = override.Tags
		}
		d.Notes = append(d.Notes, "Some fields on this endpoint were overridden in krabby and may differ from the published specification.")
	}

	if svc.BaseURL != "" {
		d.Notes = append(d.Notes, "Base URL overridden in krabby: "+svc.BaseURL)
	}

	apicatalog.BuildRecipe(d)

	detail, err := apicatalog.EncodeDetail(d)
	if err != nil {
		return err
	}

	markdown := apicatalog.Markdown(svc.Name, d)

	return emit(apicatalog.RemoteOperation{
		OpSlug:      apicatalog.OpSlug(d.Method, d.Path),
		OperationID: operationID,
		Method:      d.Method,
		Path:        d.Path,
		Summary:     d.Summary,
		Description: d.Description,
		Tags:        d.Tags,
		Deprecated:  d.Deprecated,
		Detail:      detail,
		Markdown:    markdown,
	})
}

// resolveBaseURL picks the base URL a request recipe should use: the service's
// override when set, otherwise what the document declares.
func resolveBaseURL(override, specURL string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}

	return strings.TrimRight(strings.TrimSpace(specURL), "/")
}

// firstContent picks the content type to document from a set the operation
// declares.
//
// JSON wins when it is on offer, regardless of order, because it is the type a
// generated example and a curl -d command can actually express. A spec that
// lists XML first is describing a preference of its authors, not of the caller
// reading this catalog.
func firstContent(types []string) string {
	for _, ct := range types {
		if strings.Contains(strings.ToLower(ct), "json") {
			return ct
		}
	}
	if len(types) > 0 {
		return types[0]
	}

	return ""
}

// trimText normalizes free text from a specification: specs routinely carry
// descriptions with trailing whitespace and CRLF line endings that would
// otherwise change an operation's hash between fetches from different servers.
func trimText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}
