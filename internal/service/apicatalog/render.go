package apicatalog

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// BuildRecipe fills in a Detail's Request from the rest of the Detail: an
// example body generated from the request schema, the headers the operation's
// parameters and security schemes imply, and a complete curl command.
//
// It runs at ingest, after the provider has populated everything else, so the
// generated command is stored alongside the operation rather than rebuilt on
// every read.
func BuildRecipe(d *Detail) {
	url := strings.TrimRight(d.BaseURL, "/") + d.Path

	query := requiredQuery(d.Parameters)
	if query != "" {
		url += "?" + query
	}

	headers := recipeHeaders(d)

	var body json.RawMessage
	if d.RequestBody != nil && d.RequestBody.Schema != nil {
		body = ExampleJSON(d.RequestBody.Schema)
	}

	d.Request = Recipe{
		URL:     url,
		Headers: headers,
		Body:    body,
		Command: curlCommand(d.Method, url, headers, body),
	}
}

// BuildGRPCRecipe fills in a Detail's Request with a grpcurl invocation.
//
// grpcurl rather than a Go snippet because the catalog's job is to make the
// call reproducible by hand: a reader can paste it, see the real response, and
// only then write code against it. Path carries the full method name
// ("/pkg.Service/Method"), which is also grpcurl's argument.
func BuildGRPCRecipe(d *Detail, target string, plaintext bool) {
	var body json.RawMessage
	if d.RequestBody != nil && d.RequestBody.Schema != nil {
		body = ExampleJSON(d.RequestBody.Schema)
	}

	headers := make([]string, 0, len(d.Security))
	for _, sec := range d.Security {
		if h := securityHeader(sec); h != "" {
			headers = append(headers, h)
		}
	}

	var b strings.Builder
	b.WriteString("grpcurl")
	if plaintext {
		b.WriteString(" -plaintext")
	}
	for _, h := range headers {
		// grpc metadata keys are lowercase; sending "Authorization" works but
		// echoing the HTTP spelling in a gRPC command teaches the wrong thing.
		b.WriteString(" \\\n  -H ")
		b.WriteString(shellQuote(strings.ToLower(headerName(h)) + ":" + headerValue(h)))
	}
	if len(body) > 0 {
		b.WriteString(" \\\n  -d ")
		b.WriteString(shellQuote(string(body)))
	}
	b.WriteString(" \\\n  ")
	b.WriteString(shellQuote(target))
	b.WriteString(" ")
	b.WriteString(shellQuote(strings.TrimPrefix(d.Path, "/")))

	d.Request = Recipe{
		URL:     target + d.Path,
		Headers: headers,
		Body:    body,
		Command: b.String(),
	}
}

func headerName(h string) string {
	name, _, _ := strings.Cut(h, ": ")

	return name
}

func headerValue(h string) string {
	_, value, _ := strings.Cut(h, ": ")

	return value
}

// requiredQuery renders the required query parameters as a template string.
// Optional parameters are deliberately left out: a recipe is a starting point
// that must run, and every optional parameter added to it is one more thing the
// caller has to recognise and delete.
func requiredQuery(params []Param) string {
	var parts []string
	for _, p := range params {
		if p.In != "query" || !p.Required {
			continue
		}
		parts = append(parts, p.Name+"="+placeholderFor(p))
	}

	return strings.Join(parts, "&")
}

// placeholderFor picks the most informative stand-in for a parameter: a
// documented example, then a default, then the first enum value, then the type.
func placeholderFor(p Param) string {
	switch {
	case p.Example != "":
		return p.Example
	case p.Default != "":
		return p.Default
	case len(p.Enum) > 0:
		return p.Enum[0]
	case p.Type != "":
		return "<" + p.Type + ">"
	default:
		return "<value>"
	}
}

// recipeHeaders builds the header list: required header parameters, the request
// content type, and one header per security scheme that maps to one.
func recipeHeaders(d *Detail) []string {
	var headers []string

	if d.RequestBody != nil && d.RequestBody.ContentType != "" {
		headers = append(headers, "Content-Type: "+d.RequestBody.ContentType)
	}

	for _, p := range d.Parameters {
		if p.In == "header" && p.Required {
			headers = append(headers, p.Name+": "+placeholderFor(p))
		}
	}

	for _, sec := range d.Security {
		if h := securityHeader(sec); h != "" {
			headers = append(headers, h)
		}
	}

	return headers
}

// securityHeader renders one security scheme as a header, or "" when the scheme
// is not carried in a header (an apiKey in the query string, mutual TLS).
//
// The placeholder is an environment-variable reference rather than a fake
// token so the command is safe to paste into a shell: it fails loudly with an
// empty variable instead of quietly sending a credential that looks real.
func securityHeader(sec SecurityScheme) string {
	switch strings.ToLower(sec.Type) {
	case "http":
		switch strings.ToLower(sec.Scheme) {
		case "bearer":
			return "Authorization: Bearer $TOKEN"
		case "basic":
			return "Authorization: Basic $BASIC_AUTH"
		default:
			return "Authorization: $CREDENTIAL"
		}
	case "oauth2", "openidconnect":
		return "Authorization: Bearer $TOKEN"
	case "apikey":
		if strings.EqualFold(sec.In, "header") && sec.ParamName != "" {
			return sec.ParamName + ": $API_KEY"
		}
	}

	return ""
}

// curlCommand assembles a copy-pasteable curl invocation. Single quotes are the
// outer quoting everywhere, and any single quote inside a value is escaped the
// only way POSIX shells allow: close the string, emit an escaped quote, reopen.
func curlCommand(method, url string, headers []string, body json.RawMessage) string {
	var b strings.Builder

	b.WriteString("curl -X ")
	b.WriteString(strings.ToUpper(method))
	b.WriteString(" ")
	b.WriteString(shellQuote(url))

	for _, h := range headers {
		b.WriteString(" \\\n  -H ")
		b.WriteString(shellQuote(h))
	}

	if len(body) > 0 {
		b.WriteString(" \\\n  -d ")
		b.WriteString(shellQuote(string(body)))
	}

	return b.String()
}

// shellQuote wraps s in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ExampleJSON generates an example document for a schema, preserving property
// order and honouring the same limits the schema was flattened with.
func ExampleJSON(s *Schema) json.RawMessage {
	var b strings.Builder
	writeExample(&b, s, 0)

	return json.RawMessage(b.String())
}

// writeExample emits an example value. It writes JSON directly instead of
// building a map and marshalling it, because encoding/json sorts map keys
// alphabetically and the point of the example is to mirror the order the spec
// documents — a caller comparing the example against the spec should not have
// to re-sort it in their head.
func writeExample(b *strings.Builder, s *Schema, indent int) {
	if s == nil {
		b.WriteString("null")

		return
	}

	switch schemaKind(s) {
	case "object":
		writeExampleObject(b, s, indent)
	case "array":
		b.WriteString("[")
		if s.Items != nil {
			b.WriteString("\n")
			writeIndent(b, indent+1)
			writeExample(b, s.Items, indent+1)
			b.WriteString("\n")
			writeIndent(b, indent)
		}
		b.WriteString("]")
	case "boolean":
		b.WriteString("true")
	case "integer", "number":
		b.WriteString("0")
	case "null":
		b.WriteString("null")
	default:
		b.WriteString(strconv.Quote(exampleString(s)))
	}
}

func writeExampleObject(b *strings.Builder, s *Schema, indent int) {
	if len(s.Properties) == 0 {
		b.WriteString("{}")

		return
	}

	b.WriteString("{\n")
	for i, prop := range s.Properties {
		writeIndent(b, indent+1)
		b.WriteString(strconv.Quote(prop.Name))
		b.WriteString(": ")
		writeExample(b, prop.Schema, indent+1)
		if i < len(s.Properties)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	writeIndent(b, indent)
	b.WriteString("}")
}

func writeIndent(b *strings.Builder, depth int) {
	for range depth {
		b.WriteString("  ")
	}
}

// schemaKind normalizes a schema's type, defaulting to object when the schema
// has properties but no declared type — which is what a large fraction of
// hand-written specs look like.
func schemaKind(s *Schema) string {
	if s.Type != "" {
		return strings.ToLower(s.Type)
	}
	if len(s.Properties) > 0 {
		return "object"
	}
	if s.Items != nil {
		return "array"
	}

	return "string"
}

// exampleString picks a stand-in for a string field: the first enum value when
// the field is constrained, otherwise a value shaped like the declared format.
// A format-shaped placeholder matters because it is often the only signal that
// a field is a timestamp rather than free text.
func exampleString(s *Schema) string {
	if len(s.Enum) > 0 {
		return s.Enum[0]
	}

	switch strings.ToLower(s.Format) {
	case "date-time":
		return "2024-01-01T00:00:00Z"
	case "date":
		return "2024-01-01"
	case "time":
		return "00:00:00Z"
	case "uuid":
		return "00000000-0000-0000-0000-000000000000"
	case "email":
		return "user@example.com"
	case "uri", "url":
		return "https://example.com"
	case "hostname":
		return "example.com"
	case "ipv4":
		return "127.0.0.1"
	case "byte":
		return "base64"
	case "binary":
		return "<binary>"
	case "password":
		return "$PASSWORD"
	default:
		return "string"
	}
}

// Markdown renders an operation as the document indexed into the docs RAG.
//
// It is written for retrieval, not for display: the heading carries the method,
// the path and the summary because those are the terms a question is phrased
// in, and the description, parameter names and tags follow so a semantic match
// on any of them pulls up the whole endpoint. The request recipe is included
// so a single retrieved chunk can answer "how do I call this" without a second
// lookup.
func Markdown(service string, d *Detail) string {
	var b strings.Builder

	title := d.Method + " " + d.Path
	if d.Summary != "" {
		title += " — " + d.Summary
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	fmt.Fprintf(&b, "Service: `%s`\n", service)
	if len(d.Tags) > 0 {
		fmt.Fprintf(&b, "Tags: %s\n", strings.Join(d.Tags, ", "))
	}
	if d.BaseURL != "" {
		fmt.Fprintf(&b, "Base URL: %s\n", d.BaseURL)
	}
	if d.Deprecated {
		b.WriteString("\n> **Deprecated.**\n")
	}
	b.WriteString("\n")

	if d.Description != "" {
		b.WriteString(d.Description)
		b.WriteString("\n\n")
	}

	writeParamTable(&b, d.Parameters)

	if d.RequestBody != nil {
		b.WriteString("## Request body\n\n")
		if d.RequestBody.ContentType != "" {
			fmt.Fprintf(&b, "Content type: `%s`", d.RequestBody.ContentType)
			if d.RequestBody.Required {
				b.WriteString(" (required)")
			}
			b.WriteString("\n\n")
		}
		if d.RequestBody.Description != "" {
			b.WriteString(d.RequestBody.Description)
			b.WriteString("\n\n")
		}
		if d.RequestBody.Schema != nil {
			b.WriteString("```json\n")
			b.Write(ExampleJSON(d.RequestBody.Schema))
			b.WriteString("\n```\n\n")
			writeSchemaFields(&b, d.RequestBody.Schema)
		}
	}

	writeResponses(&b, d.Responses)

	if len(d.Security) > 0 {
		b.WriteString("## Authentication\n\n")
		for _, sec := range d.Security {
			b.WriteString("- " + describeSecurity(sec) + "\n")
		}
		b.WriteString("\n")
	}

	if d.Request.Command != "" {
		b.WriteString("## Example request\n\n```sh\n")
		b.WriteString(d.Request.Command)
		b.WriteString("\n```\n\n")
	}

	if d.Truncated {
		b.WriteString("> Some schema detail was omitted to keep this document bounded; " +
			"consult the source specification for the full definition.\n")
	}

	for _, note := range d.Notes {
		b.WriteString("> " + note + "\n")
	}

	return b.String()
}

func writeParamTable(b *strings.Builder, params []Param) {
	if len(params) == 0 {
		return
	}

	b.WriteString("## Parameters\n\n")
	b.WriteString("| Name | In | Required | Type | Description |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, p := range params {
		required := ""
		if p.Required {
			required = "yes"
		}
		desc := cellText(p.Description)
		if len(p.Enum) > 0 {
			desc = strings.TrimSpace(desc + " One of: " + strings.Join(p.Enum, ", ") + ".")
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n", p.Name, p.In, required, p.Type, desc)
	}
	b.WriteString("\n")
}

// writeSchemaFields lists an object's fields under the example, so the field
// descriptions are searchable text rather than being locked inside a code
// fence where a lexical index cannot reach them usefully.
func writeSchemaFields(b *strings.Builder, s *Schema) {
	if s == nil || len(s.Properties) == 0 {
		return
	}

	b.WriteString("| Field | Type | Required | Description |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	writeSchemaRows(b, s, "")
	b.WriteString("\n")
}

func writeSchemaRows(b *strings.Builder, s *Schema, prefix string) {
	for _, prop := range s.Properties {
		name := prefix + prop.Name
		sub := prop.Schema
		if sub == nil {
			continue
		}

		required := ""
		if prop.Required {
			required = "yes"
		}
		typ := schemaKind(sub)
		if sub.Format != "" {
			typ += " (" + sub.Format + ")"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", name, typ, required, cellText(sub.Description))

		switch {
		case len(sub.Properties) > 0:
			writeSchemaRows(b, sub, name+".")
		case sub.Items != nil && len(sub.Items.Properties) > 0:
			writeSchemaRows(b, sub.Items, name+"[].")
		}
	}
}

func writeResponses(b *strings.Builder, responses []Response) {
	if len(responses) == 0 {
		return
	}

	b.WriteString("## Responses\n\n")
	for _, r := range responses {
		fmt.Fprintf(b, "### %s", r.Status)
		if r.Description != "" {
			fmt.Fprintf(b, " — %s", r.Description)
		}
		b.WriteString("\n\n")
		if r.Schema != nil {
			b.WriteString("```json\n")
			b.Write(ExampleJSON(r.Schema))
			b.WriteString("\n```\n\n")
		}
	}
}

// describeSecurity renders a scheme for human reading.
func describeSecurity(sec SecurityScheme) string {
	var b strings.Builder

	b.WriteString("`" + sec.Name + "`")
	switch strings.ToLower(sec.Type) {
	case "http":
		fmt.Fprintf(&b, " — HTTP %s authentication", sec.Scheme)
	case "apikey":
		fmt.Fprintf(&b, " — API key in %s `%s`", sec.In, sec.ParamName)
	case "oauth2":
		b.WriteString(" — OAuth2")
		if len(sec.Scopes) > 0 {
			fmt.Fprintf(&b, ", scopes: %s", strings.Join(sec.Scopes, ", "))
		}
	case "openidconnect":
		b.WriteString(" — OpenID Connect")
	case "mutualtls":
		b.WriteString(" — mutual TLS")
	default:
		if sec.Type != "" {
			b.WriteString(" — " + sec.Type)
		}
	}
	if sec.Description != "" {
		b.WriteString(". " + cellText(sec.Description))
	}

	return b.String()
}

// cellText flattens text for a markdown table cell: newlines and pipes would
// otherwise break the row.
func cellText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.Join(strings.Fields(s), " ")

	if len(s) > 300 {
		s = strings.TrimSpace(s[:300]) + "…"
	}

	return s
}

// EncodeDetail marshals a Detail, dropping schema bodies if the result exceeds
// MaxDetailBytes.
//
// The fallback drops response schemas first and the request schema last,
// because a caller building a request needs to know what to send far more than
// what comes back — and a Detail that cannot be stored at all is worse than one
// that is honest about being partial.
func EncodeDetail(d *Detail) (json.RawMessage, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("encode operation detail; %w", err)
	}
	if len(raw) <= MaxDetailBytes {
		return raw, nil
	}

	trimmed := *d
	trimmed.Truncated = true
	for i := range trimmed.Responses {
		trimmed.Responses[i].Schema = nil
	}

	raw, err = json.Marshal(&trimmed)
	if err != nil {
		return nil, fmt.Errorf("encode operation detail; %w", err)
	}
	if len(raw) <= MaxDetailBytes {
		return raw, nil
	}

	trimmed.RequestBody = nil
	trimmed.Notes = append(trimmed.Notes,
		"Schemas omitted: this operation's definition exceeds the catalog's per-operation size limit.")

	raw, err = json.Marshal(&trimmed)
	if err != nil {
		return nil, fmt.Errorf("encode operation detail; %w", err)
	}

	return raw, nil
}
