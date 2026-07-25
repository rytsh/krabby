package mcptools

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// TestNullArgPresence checks the three states a partial update needs.
func TestNullArgPresence(t *testing.T) {
	t.Parallel()

	fields, err := argFields(json.RawMessage(`{"name":"wiki","description":""}`))
	if err != nil {
		t.Fatal(err)
	}

	// Sent, even as an empty string: an intentional value.
	got := nullArg(fields, "description", "")
	if !got.Valid {
		t.Fatalf("a property sent as empty was treated as absent: %#v", got)
	}

	// Not sent at all: keep whatever is stored.
	if got := nullArg(fields, "refresh_interval", "1h"); got.Valid || got.ParsedNull {
		t.Fatalf("an absent property was treated as present: %#v", got)
	}

	// No arguments at all: everything absent.
	empty, err := argFields(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := nullArg(empty, "description", "x"); got.Valid {
		t.Fatalf("empty arguments produced a present field: %#v", got)
	}

	if _, err := argFields(json.RawMessage(`not json`)); err == nil {
		t.Fatal("malformed arguments accepted")
	}
}

// TestUpdateSourceArgsUpdate pins the tool-level behaviour: renaming a source
// must not clear its schedule.
func TestUpdateSourceArgsUpdate(t *testing.T) {
	t.Parallel()

	args := updateSourceArgs{Name: "wiki", Description: "renamed"}
	update, err := args.update(json.RawMessage(`{"name":"wiki","description":"renamed"}`))
	if err != nil {
		t.Fatal(err)
	}

	if update.Description.ValueOrZero() != "renamed" {
		t.Fatalf("description = %#v", update.Description)
	}
	if update.Specs.Valid || update.Specs.ParsedNull {
		t.Fatalf("an omitted schedule would have wiped the stored one: %#v", update.Specs)
	}
	if update.RefreshInterval.Valid || update.RefreshInterval.ParsedNull {
		t.Fatalf("an omitted refresh_interval was treated as present: %#v", update.RefreshInterval)
	}

	// A schedule that is sent is parsed and blanks dropped.
	args = updateSourceArgs{Name: "wiki", Schedule: "0 2 * * *, ,@every 6h"}
	update, err = args.update(json.RawMessage(`{"name":"wiki","schedule":"0 2 * * *, ,@every 6h"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(update.Specs.ValueOrZero(), []string{"0 2 * * *", "@every 6h"}) {
		t.Fatalf("specs = %#v", update.Specs.ValueOrZero())
	}
}

// TestToolArgSchemasStayScalar guards the reason presence is recovered from raw
// arguments instead of nullable typed fields: types.Null reflects into the
// generated schema as an object with V/Valid properties, which would ask models
// to send {"V":"...","Valid":true} instead of a plain string.
func TestToolArgSchemasStayScalar(t *testing.T) {
	t.Parallel()

	for name, check := range map[string]func(*testing.T, *jsonschema.Schema){
		"update_source": func(t *testing.T, s *jsonschema.Schema) {
			for _, field := range []string{"description", "refresh_interval", "schedule", "config"} {
				assertScalar(t, s, field)
			}
		},
	} {
		var (
			schema *jsonschema.Schema
			err    error
		)
		switch name {
		case "update_source":
			schema, err = jsonschema.For[updateSourceArgs](nil)
		}
		if err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		check(t, schema)
	}

	schema, err := jsonschema.For[upsertNamespaceArgs](nil)
	if err != nil {
		t.Fatal(err)
	}
	assertScalar(t, schema, "description")
}

func assertScalar(t *testing.T, schema *jsonschema.Schema, field string) {
	t.Helper()

	prop, ok := schema.Properties[field]
	if !ok {
		t.Fatalf("property %q missing from the tool schema", field)
	}
	if prop.Type != "string" {
		t.Fatalf("property %q has type %q, want a plain string; a nullable Go type leaked into the schema", field, prop.Type)
	}
}
