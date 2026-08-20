package mcptools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// TestSanitizeSchemaCollapsesSliceUnions guards the Gemini-compatibility fix:
// the jsonschema inferrer emits {"type": ["null","array"]} for Go slices, which
// Gemini's function-calling API rejects. sanitizeSchema must collapse those
// unions to a single "array" type before the schema is exposed over MCP.
func TestSanitizeSchemaCollapsesSliceUnions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(*jsonschema.ForOptions) (*jsonschema.Schema, error)
		field string
	}{
		{"query_graph.context_filter", jsonschema.For[queryGraphArgs], "context_filter"},
		{"refresh_repo.stages", jsonschema.For[refreshRepoArgs], "stages"},
		{"set_docs_config.docs_include", jsonschema.For[setDocsConfigArgs], "docs_include"},
		{"import_source_pages.pages", jsonschema.For[importSourcePagesArgs], "pages"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			schema, err := tt.build(nil)
			if err != nil {
				t.Fatalf("build schema: %v", err)
			}

			// Sanity check: without sanitising, the field is a nullable union.
			if got := schema.Properties[tt.field]; got == nil || len(got.Types) == 0 {
				t.Fatalf("expected %q to be a nullable union before sanitising, got %+v", tt.field, got)
			}

			sanitizeSchema(schema)

			field := schema.Properties[tt.field]
			if field.Type != "array" || len(field.Types) != 0 {
				t.Fatalf("field %q: got Type=%q Types=%v, want Type=array Types=nil", tt.field, field.Type, field.Types)
			}

			assertNoTypeUnions(t, schema)

			raw, err := json.Marshal(schema)
			if err != nil {
				t.Fatalf("marshal schema: %v", err)
			}
			if strings.Contains(string(raw), `["null"`) || strings.Contains(string(raw), `"null",`) {
				t.Fatalf("sanitised schema still contains a null type union: %s", raw)
			}
		})
	}
}

// TestRawMessageArgsAcceptJSONValues guards the call_api_endpoint fix.
//
// json.RawMessage is a []byte, so the inferrer describes it as an array of
// byte-sized integers. A validating client then refuses the only correct call —
// body {"query":"limit=1"} has type "object", want "array" — and the endpoint
// becomes uncallable. forOptions must map the type to an unconstrained schema
// so every JSON shape a body can legitimately take is accepted.
func TestRawMessageArgsAcceptJSONValues(t *testing.T) {
	t.Parallel()

	// Sanity check: the default inference really does produce the byte-array
	// schema this fix exists to replace.
	def, err := jsonschema.For[callAPIEndpointArgs](nil)
	if err != nil {
		t.Fatalf("build default schema: %v", err)
	}
	if body := def.Properties["body"]; body == nil || firstNonNull(body.Types) != "array" {
		t.Fatalf("expected the unconfigured inferrer to type body as an array, got %+v", body)
	}

	schema, err := jsonschema.For[callAPIEndpointArgs](forOptions)
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	sanitizeSchema(schema)

	body := schema.Properties["body"]
	if body == nil {
		t.Fatal("body property missing from call_api_endpoint schema")
	}
	if body.Type != "" || len(body.Types) != 0 || body.Items != nil {
		t.Fatalf("body must not constrain its type, got Type=%q Types=%v Items=%+v", body.Type, body.Types, body.Items)
	}

	// The per-field jsonschema tag must still win over the type mapping, or
	// the argument loses its documentation.
	if !strings.Contains(body.Description, "protojson") {
		t.Errorf("body lost its field description, got %q", body.Description)
	}

	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}

	// Every shape a real body takes must validate: a unary gRPC message, a
	// client-streaming sequence, an HTTP scalar and an absent body.
	bodies := []string{
		`{"query":"limit=1"}`,
		`[{"query":"a"},{"query":"b"}]`,
		`"raw text"`,
		`null`,
	}
	for _, b := range bodies {
		var args any
		payload := `{"service":"transaction","endpoint":"/v1.S/M","body":` + b + `}`
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			t.Fatalf("unmarshal %s: %v", payload, err)
		}
		if err := resolved.Validate(args); err != nil {
			t.Errorf("body %s rejected by the advertised schema: %v", b, err)
		}
	}
}

// TestRawMessageArgsAreUnconstrainedEverywhere guards arbitrary request bodies.
// Admin config and merge-patch fields use jsonObject instead because strict MCP
// clients require a concrete type for every property.
func TestRawMessageArgsAreUnconstrainedEverywhere(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		build  func(*jsonschema.ForOptions) (*jsonschema.Schema, error)
		fields []string
	}{
		{"call_api_endpoint", jsonschema.For[callAPIEndpointArgs], []string{"body"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			schema, err := tt.build(forOptions)
			if err != nil {
				t.Fatalf("build schema: %v", err)
			}
			sanitizeSchema(schema)

			for _, name := range tt.fields {
				field := schema.Properties[name]
				if field == nil {
					t.Fatalf("property %q missing", name)
				}
				if field.Type != "" || len(field.Types) != 0 {
					t.Errorf("property %q constrains its type: Type=%q Types=%v", name, field.Type, field.Types)
				}
				if field.Items != nil {
					t.Errorf("property %q still describes byte items: %+v", name, field.Items)
				}
				if field.Description == "" {
					t.Errorf("property %q has no description", name)
				}
			}
		})
	}
}

func TestJSONObjectAcceptsOnlyObjects(t *testing.T) {
	t.Parallel()

	var object jsonObject
	if err := json.Unmarshal([]byte(`{"url":"https://example.test/openapi.json"}`), &object); err != nil {
		t.Fatalf("object rejected: %v", err)
	}
	if got := string(object); got != `{"url":"https://example.test/openapi.json"}` {
		t.Fatalf("stored object = %s", got)
	}

	for _, input := range []string{`[]`, `"object"`, `1`, `true`} {
		if err := json.Unmarshal([]byte(input), &object); err == nil {
			t.Errorf("non-object %s was accepted", input)
		}
	}

	if err := json.Unmarshal([]byte(`null`), &object); err != nil || object != nil {
		t.Fatalf("null did not clear the object: value=%s err=%v", object, err)
	}
}

// assertNoTypeUnions fails if any schema node in the tree still carries a
// multi-valued "type" (the shape Gemini rejects).
func assertNoTypeUnions(t *testing.T, s *jsonschema.Schema) {
	t.Helper()

	if s == nil {
		return
	}
	if len(s.Types) > 0 {
		t.Fatalf("schema node still has a type union: %v", s.Types)
	}

	assertNoTypeUnions(t, s.Items)
	assertNoTypeUnions(t, s.AdditionalProperties)
	for _, sub := range s.Properties {
		assertNoTypeUnions(t, sub)
	}
	for _, sub := range s.PrefixItems {
		assertNoTypeUnions(t, sub)
	}
	for _, sub := range s.ItemsArray {
		assertNoTypeUnions(t, sub)
	}
}
