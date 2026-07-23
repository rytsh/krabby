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
