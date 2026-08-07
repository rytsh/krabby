package openapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

const specV3 = `{
  "openapi": "3.0.3",
  "info": {"title": "Billing API", "version": "2.1.0", "description": "Invoices and payments."},
  "servers": [{"url": "https://billing.example.com/api"}],
  "components": {
    "securitySchemes": {
      "bearerAuth": {"type": "http", "scheme": "bearer"}
    },
    "schemas": {
      "Money": {
        "type": "object",
        "required": ["amount"],
        "properties": {
          "amount": {"type": "integer", "description": "Minor units."},
          "currency": {"type": "string", "enum": ["EUR", "TRY"]}
        }
      },
      "Invoice": {
        "allOf": [
          {"type": "object", "properties": {"id": {"type": "string", "format": "uuid"}}},
          {
            "type": "object",
            "required": ["customer_id"],
            "properties": {
              "customer_id": {"type": "string"},
              "total": {"$ref": "#/components/schemas/Money"},
              "issued_at": {"type": "string", "format": "date-time"}
            }
          }
        ]
      }
    }
  },
  "security": [{"bearerAuth": []}],
  "paths": {
    "/v1/invoices": {
      "post": {
        "operationId": "createInvoice",
        "summary": "Create an invoice",
        "tags": ["invoices"],
        "requestBody": {
          "required": true,
          "content": {
            "application/xml": {"schema": {"type": "string"}},
            "application/json": {"schema": {"$ref": "#/components/schemas/Invoice"}}
          }
        },
        "responses": {
          "201": {
            "description": "Created",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Invoice"}}}
          }
        }
      }
    },
    "/v1/invoices/{id}": {
      "parameters": [
        {"name": "id", "in": "path", "schema": {"type": "string"}, "description": "Invoice id."}
      ],
      "get": {
        "operationId": "getInvoice",
        "summary": "Fetch one invoice",
        "tags": ["invoices"],
        "parameters": [
          {"name": "expand", "in": "query", "required": true, "schema": {"type": "string", "enum": ["lines"]}}
        ],
        "responses": {"200": {"description": "OK"}}
      }
    }
  }
}`

const specV2 = `{
  "swagger": "2.0",
  "info": {"title": "Legacy API", "version": "1.0.0"},
  "host": "legacy.example.com",
  "basePath": "/v1",
  "schemes": ["https"],
  "consumes": ["application/json"],
  "produces": ["application/json"],
  "securityDefinitions": {
    "api_key": {"type": "apiKey", "name": "X-Api-Key", "in": "header"}
  },
  "security": [{"api_key": []}],
  "definitions": {
    "Customer": {
      "type": "object",
      "required": ["name"],
      "properties": {
        "name": {"type": "string"},
        "age": {"type": "integer"}
      }
    }
  },
  "paths": {
    "/customers": {
      "post": {
        "operationId": "createCustomer",
        "summary": "Create a customer",
        "tags": ["customers"],
        "parameters": [
          {"name": "body", "in": "body", "required": true, "schema": {"$ref": "#/definitions/Customer"}},
          {"name": "dry_run", "in": "query", "type": "boolean"}
        ],
        "responses": {"200": {"description": "OK", "schema": {"$ref": "#/definitions/Customer"}}}
      }
    }
  }
}`

// specCyclic models a self-referencing type, the shape that makes a naive
// "expand every $ref" walker run forever.
const specCyclic = `{
  "openapi": "3.0.3",
  "info": {"title": "Tree", "version": "1.0.0"},
  "components": {
    "schemas": {
      "Node": {
        "type": "object",
        "properties": {
          "name": {"type": "string"},
          "children": {"type": "array", "items": {"$ref": "#/components/schemas/Node"}},
          "parent": {"$ref": "#/components/schemas/Node"}
        }
      }
    }
  },
  "paths": {
    "/tree": {
      "post": {
        "operationId": "putTree",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Node"}}}},
        "responses": {"200": {"description": "OK"}}
      }
    }
  }
}`

// specServer serves a document and records what was requested, so conditional
// fetches and auth headers can be asserted.
type specServer struct {
	*httptest.Server
	requests int
	auth     string
	inm      string
	etag     string
}

func newSpecServer(t *testing.T, body string) *specServer {
	t.Helper()

	s := &specServer{etag: `"v1"`}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests++
		s.auth = r.Header.Get("Authorization")
		s.inm = r.Header.Get("If-None-Match")

		w.Header().Set("ETag", s.etag)
		if r.Header.Get("If-None-Match") == s.etag {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)

	return s
}

// collect runs one fetch and returns the emitted operations keyed by operation
// id.
func collect(t *testing.T, p *Provider, svc *apicatalog.Service) (map[string]apicatalog.RemoteOperation, *apicatalog.FetchResult) {
	t.Helper()

	ops := map[string]apicatalog.RemoteOperation{}
	res, err := p.Fetch(context.Background(), svc, svc.State, func(op apicatalog.RemoteOperation) error {
		ops[op.OperationID] = op

		return nil
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	return ops, res
}

func serviceFor(name, url string) *apicatalog.Service {
	return &apicatalog.Service{
		Name:   name,
		Kind:   apicatalog.KindOpenAPI,
		Config: json.RawMessage(fmt.Sprintf(`{"url":%q}`, url)),
	}
}

func decodeDetail(t *testing.T, op apicatalog.RemoteOperation) apicatalog.Detail {
	t.Helper()

	var d apicatalog.Detail
	if err := json.Unmarshal(op.Detail, &d); err != nil {
		t.Fatalf("decode detail of %s; %v", op.OperationID, err)
	}

	return d
}

func TestFetchV3(t *testing.T) {
	server := newSpecServer(t, specV3)
	svc := serviceFor("billing", server.URL)

	ops, res := collect(t, New(), svc)

	if !res.Complete {
		t.Fatalf("Complete = false, want true for a full walk")
	}
	if res.Info.Title != "Billing API" || res.Info.Version != "2.1.0" {
		t.Fatalf("Info = %+v, want title/version from the document", res.Info)
	}
	if res.Info.ResolvedURL != "https://billing.example.com/api" {
		t.Fatalf("ResolvedURL = %q, want the document server", res.Info.ResolvedURL)
	}
	if len(ops) != 2 {
		t.Fatalf("emitted %d operations, want 2: %v", len(ops), keys(ops))
	}

	create, ok := ops["createInvoice"]
	if !ok {
		t.Fatalf("createInvoice not emitted; got %v", keys(ops))
	}
	if create.Method != "POST" || create.Path != "/v1/invoices" {
		t.Fatalf("createInvoice = %s %s, want POST /v1/invoices", create.Method, create.Path)
	}

	d := decodeDetail(t, create)

	// JSON must win over the XML content type declared first in the document.
	if d.RequestBody == nil || d.RequestBody.ContentType != "application/json" {
		t.Fatalf("request body content type = %+v, want application/json", d.RequestBody)
	}
	if !d.RequestBody.Required {
		t.Errorf("request body Required = false, want true")
	}

	// allOf members must be merged into one flat object, and the $ref inside
	// one of them resolved.
	fields := propertyNames(d.RequestBody.Schema)
	for _, want := range []string{"id", "customer_id", "total", "issued_at"} {
		if !fields[want] {
			t.Errorf("request schema is missing merged allOf field %q; got %v", want, keys(fields))
		}
	}

	total := findProperty(d.RequestBody.Schema, "total")
	if total == nil || total.Schema == nil {
		t.Fatalf("total property missing")
	}
	if !propertyNames(total.Schema)["amount"] {
		t.Errorf("$ref to Money was not resolved; total = %+v", total.Schema)
	}

	if !requiredHas(d.RequestBody.Schema, "customer_id") {
		t.Errorf("required from an allOf member was not merged; got %v", d.RequestBody.Schema.Required)
	}

	// The example body must reflect declared formats, not just "string". It is
	// compacted on the way into storage (encoding/json compacts a RawMessage),
	// so the stored form has no spaces; the readable form lives in the command
	// and in the markdown projection.
	body := string(d.Request.Body)
	if !strings.Contains(body, `"issued_at":"2024-01-01T00:00:00Z"`) {
		t.Errorf("example body did not use the date-time format:\n%s", body)
	}
	if !strings.Contains(d.Request.Command, `"issued_at": "2024-01-01T00:00:00Z"`) {
		t.Errorf("curl command body was not pretty-printed:\n%s", d.Request.Command)
	}

	if !strings.Contains(d.Request.Command, "curl -X POST 'https://billing.example.com/api/v1/invoices'") {
		t.Errorf("curl command = %q, want the resolved URL", d.Request.Command)
	}
	if !strings.Contains(d.Request.Command, "Authorization: Bearer $TOKEN") {
		t.Errorf("curl command did not apply the global bearer security:\n%s", d.Request.Command)
	}
}

func TestFetchV3PathParameters(t *testing.T) {
	server := newSpecServer(t, specV3)
	svc := serviceFor("billing", server.URL)

	ops, _ := collect(t, New(), svc)

	get, ok := ops["getInvoice"]
	if !ok {
		t.Fatalf("getInvoice not emitted; got %v", keys(ops))
	}

	d := decodeDetail(t, get)

	var id, expand *apicatalog.Param
	for i := range d.Parameters {
		switch d.Parameters[i].Name {
		case "id":
			id = &d.Parameters[i]
		case "expand":
			expand = &d.Parameters[i]
		}
	}

	if id == nil {
		t.Fatalf("path-level parameter was not inherited by the operation; got %+v", d.Parameters)
	}
	// The document does not mark it required; a path parameter always is.
	if !id.Required {
		t.Errorf("path parameter id Required = false, want true")
	}
	if expand == nil || len(expand.Enum) != 1 || expand.Enum[0] != "lines" {
		t.Fatalf("query parameter enum not carried; expand = %+v", expand)
	}

	// A required query parameter belongs in the recipe; the path template does
	// not get substituted, so the caller can see what to fill in.
	if !strings.Contains(d.Request.URL, "/v1/invoices/{id}?expand=lines") {
		t.Errorf("recipe URL = %q, want the templated path and the required query", d.Request.URL)
	}
}

func TestFetchV2(t *testing.T) {
	server := newSpecServer(t, specV2)
	svc := serviceFor("legacy", server.URL)

	ops, res := collect(t, New(), svc)

	if res.Info.ResolvedURL != "https://legacy.example.com/v1" {
		t.Fatalf("ResolvedURL = %q, want scheme+host+basePath", res.Info.ResolvedURL)
	}

	create, ok := ops["createCustomer"]
	if !ok {
		t.Fatalf("createCustomer not emitted; got %v", keys(ops))
	}

	d := decodeDetail(t, create)

	// The in:"body" parameter must become the request body, not a parameter.
	for _, p := range d.Parameters {
		if p.In == "body" {
			t.Fatalf("in:body parameter leaked into the parameter list: %+v", p)
		}
	}
	if d.RequestBody == nil || d.RequestBody.Schema == nil {
		t.Fatalf("in:body parameter was not turned into a request body")
	}
	if !propertyNames(d.RequestBody.Schema)["name"] {
		t.Errorf("$ref to a swagger definition was not resolved: %+v", d.RequestBody.Schema)
	}
	if len(d.Parameters) != 1 || d.Parameters[0].Name != "dry_run" {
		t.Fatalf("parameters = %+v, want only dry_run", d.Parameters)
	}

	// Swagger's apiKey must map onto the header the recipe sets.
	if !strings.Contains(d.Request.Command, "X-Api-Key: $API_KEY") {
		t.Errorf("curl command did not apply the apiKey security:\n%s", d.Request.Command)
	}
}

func TestFetchCyclicSchemaTerminates(t *testing.T) {
	server := newSpecServer(t, specCyclic)
	svc := serviceFor("tree", server.URL)

	ops, _ := collect(t, New(), svc)

	op, ok := ops["putTree"]
	if !ok {
		t.Fatalf("putTree not emitted; got %v", keys(ops))
	}

	d := decodeDetail(t, op)
	if d.RequestBody == nil || d.RequestBody.Schema == nil {
		t.Fatalf("request body missing")
	}
	if !d.Truncated {
		t.Errorf("Truncated = false, want true: a cyclic schema must be reported as cut")
	}

	parent := findProperty(d.RequestBody.Schema, "parent")
	if parent == nil || parent.Schema == nil {
		t.Fatalf("parent property missing")
	}
	// The cycle must be stubbed by name rather than expanded again.
	if !parent.Schema.Truncated || parent.Schema.Ref != "Node" {
		t.Errorf("cyclic ref = %+v, want a truncated stub naming Node", parent.Schema)
	}
}

func TestSpecPatchOverride(t *testing.T) {
	server := newSpecServer(t, specV3)
	svc := serviceFor("billing", server.URL)
	// Rewrite a schema field's type and delete another, the two things a merge
	// patch has to be able to do.
	svc.SpecPatch = json.RawMessage(`{
      "components": {"schemas": {"Money": {"properties": {
        "amount": {"type": "string", "description": "Patched."},
        "currency": null
      }}}}
    }`)

	ops, _ := collect(t, New(), svc)

	d := decodeDetail(t, ops["createInvoice"])
	total := findProperty(d.RequestBody.Schema, "total")
	if total == nil || total.Schema == nil {
		t.Fatalf("total property missing")
	}

	amount := findProperty(total.Schema, "amount")
	if amount == nil || amount.Schema == nil {
		t.Fatalf("amount property missing")
	}
	if amount.Schema.Type != "string" {
		t.Errorf("patched amount type = %q, want string", amount.Schema.Type)
	}
	if amount.Schema.Description != "Patched." {
		t.Errorf("patched amount description = %q, want %q", amount.Schema.Description, "Patched.")
	}
	if findProperty(total.Schema, "currency") != nil {
		t.Errorf("null in a merge patch did not delete the currency property")
	}
}

func TestBaseURLAndOperationOverrides(t *testing.T) {
	server := newSpecServer(t, specV3)
	svc := serviceFor("billing", server.URL)
	svc.BaseURL = "https://billing.internal.corp"
	svc.Operations = map[string]apicatalog.OperationOverride{
		"createInvoice": {Summary: "Raise a new invoice", Tags: []string{"internal"}},
		"getInvoice":    {Hidden: true},
	}

	ops, _ := collect(t, New(), svc)

	if _, hidden := ops["getInvoice"]; hidden {
		t.Errorf("a hidden operation was still emitted")
	}
	if len(ops) != 1 {
		t.Fatalf("emitted %d operations, want 1 after hiding one: %v", len(ops), keys(ops))
	}

	create := ops["createInvoice"]
	if create.Summary != "Raise a new invoice" {
		t.Errorf("summary override not applied: %q", create.Summary)
	}
	if len(create.Tags) != 1 || create.Tags[0] != "internal" {
		t.Errorf("tag override not applied: %v", create.Tags)
	}

	d := decodeDetail(t, create)
	if d.BaseURL != "https://billing.internal.corp" {
		t.Errorf("BaseURL = %q, want the override", d.BaseURL)
	}
	if !strings.Contains(d.Request.Command, "https://billing.internal.corp/v1/invoices") {
		t.Errorf("recipe did not use the overridden base URL:\n%s", d.Request.Command)
	}
	if !strings.Contains(create.Markdown, "Raise a new invoice") {
		t.Errorf("markdown projection did not carry the overridden summary")
	}
}

// TestOverrideByMethodPath locks in the fallback override key. operationId is
// optional and unstable, so an override keyed by "METHOD /path" has to work.
func TestOverrideByMethodPath(t *testing.T) {
	server := newSpecServer(t, specV3)
	svc := serviceFor("billing", server.URL)
	svc.Operations = map[string]apicatalog.OperationOverride{
		"POST /v1/invoices": {Summary: "By path"},
	}

	ops, _ := collect(t, New(), svc)
	if got := ops["createInvoice"].Summary; got != "By path" {
		t.Errorf("summary = %q, want the override keyed by METHOD /path", got)
	}
}

func TestConditionalFetch(t *testing.T) {
	server := newSpecServer(t, specV3)
	svc := serviceFor("billing", server.URL)

	_, res := collect(t, New(), svc)
	if res.Unchanged {
		t.Fatalf("first fetch reported Unchanged")
	}
	svc.State = res.State

	provider := New()
	ops, res := collect(t, provider, svc)
	if !res.Unchanged {
		t.Fatalf("second fetch of an unmodified document reported changed")
	}
	if len(ops) != 0 {
		t.Errorf("an unchanged fetch emitted %d operations, want 0", len(ops))
	}
	if server.inm != `"v1"` {
		t.Errorf("If-None-Match = %q, want the stored ETag", server.inm)
	}
}

// TestUnchangedByFingerprint covers the common internal case: a server that
// sends no validators at all. The content hash has to stand in for them, or a
// static document is re-embedded on every poll.
func TestUnchangedByFingerprint(t *testing.T) {
	server := newSpecServer(t, specV3)
	server.etag = ""
	svc := serviceFor("billing", server.URL)

	_, res := collect(t, New(), svc)
	svc.State = res.State

	_, res = collect(t, New(), svc)
	if !res.Unchanged {
		t.Fatalf("identical bytes with no ETag were not detected as unchanged")
	}
}

func TestFetchAuth(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{name: "bearer", config: `{"url":%q,"token":"secret"}`, want: "Bearer secret"},
		{name: "basic", config: `{"url":%q,"user":"u","token":"p"}`, want: "Basic dTpw"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newSpecServer(t, specV3)
			svc := &apicatalog.Service{
				Name:   "billing",
				Kind:   apicatalog.KindOpenAPI,
				Config: json.RawMessage(fmt.Sprintf(tt.config, server.URL)),
			}

			collect(t, New(), svc)

			if server.auth != tt.want {
				t.Errorf("Authorization = %q, want %q", server.auth, tt.want)
			}
		})
	}
}

// TestConfigViewRedactsToken locks in the repo-wide rule that a secret must
// never survive a round trip through the API.
func TestConfigViewRedactsToken(t *testing.T) {
	p := New()

	raw, err := p.MergeConfig(nil, json.RawMessage(`{"url":"https://x/openapi.json","token":"s3cret"}`))
	if err != nil {
		t.Fatalf("MergeConfig() error = %v", err)
	}

	view, err := json.Marshal(p.ConfigView(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(view), "s3cret") {
		t.Fatalf("ConfigView leaked the token: %s", view)
	}
	if !strings.Contains(string(view), `"token_set":true`) {
		t.Errorf("ConfigView did not report that a token is stored: %s", view)
	}
}

// TestMergeConfigKeepsToken covers the write-only rule: the UI always posts the
// whole config with a blank token, and that must not erase the stored one.
func TestMergeConfigKeepsToken(t *testing.T) {
	p := New()

	stored, err := p.MergeConfig(nil, json.RawMessage(`{"url":"https://x/openapi.json","token":"keep"}`))
	if err != nil {
		t.Fatalf("MergeConfig() error = %v", err)
	}

	merged, err := p.MergeConfig(stored, json.RawMessage(`{"url":"https://y/openapi.json","token":""}`))
	if err != nil {
		t.Fatalf("MergeConfig() error = %v", err)
	}

	cfg, err := decodeConfig(merged)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "keep" {
		t.Errorf("token = %q, want the stored secret preserved", cfg.Token)
	}
	if cfg.URL != "https://y/openapi.json" {
		t.Errorf("url = %q, want the update applied", cfg.URL)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{name: "ok", config: `{"url":"https://x/openapi.json"}`},
		{name: "missing url", config: `{}`, wantErr: true},
		{name: "not http", config: `{"url":"ftp://x/spec"}`, wantErr: true},
		{name: "blank header name", config: `{"url":"https://x","headers":{"  ":"v"}}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := New().Validate(json.RawMessage(tt.config))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPreview(t *testing.T) {
	server := newSpecServer(t, specV3)

	out, err := New().Preview(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"url":%q}`, server.URL)), nil)
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}

	if out.Title != "Billing API" {
		t.Errorf("Title = %q, want Billing API", out.Title)
	}
	if out.OperationCount != 2 {
		t.Errorf("OperationCount = %d, want 2", out.OperationCount)
	}
	if len(out.Sample) != 2 {
		t.Errorf("Sample = %v, want two entries", out.Sample)
	}
}

// ---- helpers ---------------------------------------------------------------

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func propertyNames(s *apicatalog.Schema) map[string]bool {
	out := map[string]bool{}
	if s == nil {
		return out
	}
	for _, p := range s.Properties {
		out[p.Name] = true
	}

	return out
}

func findProperty(s *apicatalog.Schema, name string) *apicatalog.Property {
	if s == nil {
		return nil
	}
	for i := range s.Properties {
		if s.Properties[i].Name == name {
			return &s.Properties[i]
		}
	}

	return nil
}

func requiredHas(s *apicatalog.Schema, name string) bool {
	if s == nil {
		return false
	}
	for _, r := range s.Required {
		if r == name {
			return true
		}
	}

	return false
}
