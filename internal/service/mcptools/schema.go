package mcptools

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addTool registers a tool exactly like mcp.AddTool, but replaces the
// reflection-inferred input schema with one sanitised for the strictest MCP
// clients (notably Google Gemini).
//
// The upstream jsonschema inferrer emits a nullable union type for every Go
// slice (and pointer): a []string field becomes
//
//	{"type": ["null", "array"], "items": {"type": "string"}}
//
// That is valid JSON Schema, but Gemini's function-calling API only accepts a
// single string "type" (an OpenAPI 3.0 subset). When a client forwards such a
// schema to Gemini, it rejects the ENTIRE tools payload - not just the offending
// field - with errors like:
//
//	properties[context_filter].items: field predicate failed: $type == Type.ARRAY
//	properties[context_filter].any_of[0].items: missing field
//
// Sanitising here keeps every tool krabby exposes portable across MCP clients no
// matter how strict their schema converter is.
func addTool[In, Out any](server *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if t.InputSchema == nil {
		schema, err := jsonschema.For[In](nil)
		if err != nil {
			panic(fmt.Sprintf("mcptools: build input schema for tool %q: %v", t.Name, err))
		}

		sanitizeSchema(schema)
		t.InputSchema = schema
	}

	mcp.AddTool(server, t, h)
}

// sanitizeSchema walks a schema tree in place and rewrites it to the subset the
// strictest MCP clients accept. Currently it collapses a nullable/multi type
// union ("type": ["null", "array"]) down to the single non-null type
// ("type": "array"). Every optional field krabby exposes is already omitempty,
// so dropping the "null" branch loses nothing meaningful.
func sanitizeSchema(s *jsonschema.Schema) {
	if s == nil {
		return
	}

	if len(s.Types) > 0 {
		s.Type = firstNonNull(s.Types)
		s.Types = nil
	}

	sanitizeSchema(s.Items)
	sanitizeSchema(s.AdditionalProperties)
	sanitizeSchema(s.Contains)
	sanitizeSchema(s.Not)
	sanitizeSchema(s.PropertyNames)

	sanitizeSchemas(s.PrefixItems)
	sanitizeSchemas(s.ItemsArray)
	sanitizeSchemas(s.AllOf)
	sanitizeSchemas(s.AnyOf)
	sanitizeSchemas(s.OneOf)

	sanitizeSchemaMap(s.Properties)
	sanitizeSchemaMap(s.PatternProperties)
	sanitizeSchemaMap(s.Defs)
	sanitizeSchemaMap(s.Definitions)
}

func sanitizeSchemas(list []*jsonschema.Schema) {
	for _, sub := range list {
		sanitizeSchema(sub)
	}
}

func sanitizeSchemaMap(m map[string]*jsonschema.Schema) {
	for _, sub := range m {
		sanitizeSchema(sub)
	}
}

// firstNonNull returns the first non-"null" entry of a JSON Schema type union,
// or "" when the union carries only "null".
func firstNonNull(types []string) string {
	for _, t := range types {
		if t != "null" {
			return t
		}
	}

	return ""
}
