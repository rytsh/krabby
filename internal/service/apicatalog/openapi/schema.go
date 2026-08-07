package openapi

import (
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// flattener turns libopenapi's schema graph into the catalog's bounded tree.
//
// It is shared by the Swagger 2.0 and OpenAPI 3.x walkers, which is the whole
// reason both versions are supported for the price of one: the two documents
// differ in where a schema hangs (a body parameter versus a request body, a
// response.schema versus a response.content[type].schema), but the schema
// objects themselves are the same libopenapi type. Only the plumbing is
// per-version; the hard part is written once.
type flattener struct {
	// truncated records that some limit cut a branch anywhere in this
	// operation, so the Detail can carry one honest flag.
	truncated bool
	// path holds the $ref names currently being expanded, so a schema that
	// reaches itself is stubbed rather than followed forever.
	path map[string]bool
}

func newFlattener() *flattener {
	return &flattener{path: map[string]bool{}}
}

// schema flattens a schema proxy at the given depth.
//
// Two independent guards bound the walk, because they catch different things.
// The depth limit bounds legitimately deep nesting, which terminates on its own
// but produces more than a reader can use. The ref-path check catches genuine
// cycles — a Node with a children array of Nodes — which the depth limit would
// also stop, but only after expanding four useless copies of the same type.
// Stubbing the cycle at its first repeat says something truer: "this is the
// type you already saw".
func (f *flattener) schema(proxy *base.SchemaProxy, depth int) *apicatalog.Schema {
	if proxy == nil {
		return nil
	}

	ref := refName(proxy)

	if depth > apicatalog.MaxSchemaDepth {
		f.truncated = true

		return &apicatalog.Schema{Ref: ref, Truncated: true}
	}

	if ref != "" && f.path[ref] {
		// A cycle: name the type instead of expanding it again. This counts as
		// truncation even though nothing was "cut" by a limit — a recursive
		// type can never be fully expanded, and the reader needs to be told to
		// consult the specification rather than assume the stub is the whole
		// definition.
		f.truncated = true

		return &apicatalog.Schema{Ref: ref, Type: "object", Truncated: true}
	}

	resolved := proxy.Schema()
	if resolved == nil {
		return &apicatalog.Schema{Ref: ref}
	}

	if ref != "" {
		f.path[ref] = true
		defer delete(f.path, ref)
	}

	return f.build(resolved, ref, depth)
}

func (f *flattener) build(s *base.Schema, ref string, depth int) *apicatalog.Schema {
	out := &apicatalog.Schema{
		Ref:         ref,
		Format:      s.Format,
		Description: strings.TrimSpace(s.Description),
		Required:    s.Required,
		Enum:        nodeStrings(s.Enum),
	}

	if len(s.Type) > 0 {
		out.Type = s.Type[0]
	}
	if s.Nullable != nil {
		out.Nullable = *s.Nullable
	}
	if out.Description == "" && s.Title != "" {
		out.Description = strings.TrimSpace(s.Title)
	}

	// allOf is composition, not nesting: its members' fields belong to *this*
	// object. Merging at the same depth is what makes a spec that models
	// inheritance with allOf read like the flat object a caller actually sends.
	merged := f.mergeComposition(s, depth)

	required := map[string]bool{}
	for _, name := range out.Required {
		required[name] = true
	}
	for _, name := range merged.required {
		required[name] = true
		out.Required = appendUnique(out.Required, name)
	}

	out.Properties = f.properties(s, merged.properties, required, depth)

	if s.Items != nil && s.Items.N == 0 && s.Items.A != nil {
		out.Items = f.schema(s.Items.A, depth+1)
	}

	if out.Type == "" {
		if len(out.Properties) > 0 {
			out.Type = "object"
		} else if out.Items != nil {
			out.Type = "array"
		}
	}

	return out
}

// composition is the flattened contribution of allOf/oneOf/anyOf members.
type composition struct {
	properties []namedSchema
	required   []string
}

type namedSchema struct {
	name  string
	proxy *base.SchemaProxy
}

// mergeComposition collects the fields contributed by allOf, and — when the
// schema has no fields of its own — by the first oneOf/anyOf variant.
//
// Taking only the first variant is a deliberate simplification. A request
// example has to be one concrete document, and a reader who needs the
// alternatives has the spec; producing a union that is valid against none of
// the branches would be worse than showing one that is valid against one.
func (f *flattener) mergeComposition(s *base.Schema, depth int) composition {
	var out composition

	for _, member := range s.AllOf {
		f.collect(member, depth, &out)
	}

	if len(out.properties) == 0 && s.Properties == nil {
		switch {
		case len(s.OneOf) > 0:
			f.collect(s.OneOf[0], depth, &out)
		case len(s.AnyOf) > 0:
			f.collect(s.AnyOf[0], depth, &out)
		}
	}

	return out
}

// collect adds one composition member's properties to out, recursing through
// nested allOf. The ref-path guard in schema() does not cover this walk, so the
// cycle check is repeated here.
func (f *flattener) collect(proxy *base.SchemaProxy, depth int, out *composition) {
	if proxy == nil {
		return
	}

	ref := refName(proxy)
	if ref != "" && f.path[ref] {
		f.truncated = true

		return
	}
	if ref != "" {
		f.path[ref] = true
		defer delete(f.path, ref)
	}

	member := proxy.Schema()
	if member == nil {
		return
	}

	out.required = append(out.required, member.Required...)

	if member.Properties != nil {
		for name, sub := range member.Properties.FromOldest() {
			out.properties = append(out.properties, namedSchema{name: name, proxy: sub})
		}
	}

	for _, nested := range member.AllOf {
		f.collect(nested, depth, out)
	}
}

// properties flattens the schema's own properties followed by the ones
// inherited through composition, dropping duplicates and stopping at the limit.
//
// Own properties come first because a schema that redeclares an inherited field
// is narrowing it, and the narrowed definition is the one that applies.
func (f *flattener) properties(s *base.Schema, inherited []namedSchema, required map[string]bool, depth int) []apicatalog.Property {
	var ordered []namedSchema

	if s.Properties != nil {
		for name, sub := range s.Properties.FromOldest() {
			ordered = append(ordered, namedSchema{name: name, proxy: sub})
		}
	}
	ordered = append(ordered, inherited...)

	if len(ordered) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(ordered))
	out := make([]apicatalog.Property, 0, len(ordered))
	for _, item := range ordered {
		if seen[item.name] {
			continue
		}
		seen[item.name] = true

		if len(out) >= apicatalog.MaxSchemaProperties {
			f.truncated = true

			break
		}

		out = append(out, apicatalog.Property{
			Name:     item.name,
			Required: required[item.name],
			Schema:   f.schema(item.proxy, depth+1),
		})
	}

	return out
}

// refName returns the component name a proxy points at ("Invoice" for
// "#/components/schemas/Invoice"), or "" for an inline schema.
func refName(proxy *base.SchemaProxy) string {
	if proxy == nil || !proxy.IsReference() {
		return ""
	}

	ref := proxy.GetReference()
	if idx := strings.LastIndex(ref, "/"); idx >= 0 {
		return ref[idx+1:]
	}

	return ref
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}

	return append(list, value)
}
