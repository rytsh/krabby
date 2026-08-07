package grpcreflect

import (
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// flattener turns a protobuf message descriptor into the catalog's bounded
// schema tree.
//
// Protobuf message graphs cycle as readily as JSON Schema ones — a Node with a
// repeated Node field is idiomatic — so the same two guards apply: a depth cap
// for legitimately deep nesting and a message-path check that stubs a type the
// moment it reaches itself.
type flattener struct {
	truncated bool
	path      map[protoreflect.FullName]bool
}

func newFlattener() *flattener {
	return &flattener{path: map[protoreflect.FullName]bool{}}
}

// message flattens a message descriptor at the given depth.
func (f *flattener) message(md protoreflect.MessageDescriptor, depth int) *apicatalog.Schema {
	if md == nil {
		return nil
	}

	name := md.FullName()
	short := string(name.Name())

	if depth > apicatalog.MaxSchemaDepth {
		f.truncated = true

		return &apicatalog.Schema{Type: "object", Ref: short, Truncated: true}
	}
	if f.path[name] {
		f.truncated = true

		return &apicatalog.Schema{Type: "object", Ref: short, Truncated: true}
	}

	f.path[name] = true
	defer delete(f.path, name)

	out := &apicatalog.Schema{Type: "object", Ref: short}

	fields := md.Fields()
	for i := range fields.Len() {
		if len(out.Properties) >= apicatalog.MaxSchemaProperties {
			f.truncated = true

			break
		}

		fd := fields.Get(i)
		out.Properties = append(out.Properties, apicatalog.Property{
			Name: string(fd.Name()),
			// proto3 has no required fields, and marking everything optional
			// would be as useless as marking nothing. The catalog reports a
			// field as required only when the descriptor actually says so,
			// which in practice means proto2 documents.
			Required: fd.Cardinality() == protoreflect.Required,
			Schema:   f.field(fd, depth),
		})
	}

	return out
}

// field flattens one field, wrapping repeated fields in an array and mapping
// map fields to an object keyed by the map's value type.
func (f *flattener) field(fd protoreflect.FieldDescriptor, depth int) *apicatalog.Schema {
	if fd.IsMap() {
		// A protobuf map has no JSON-Schema equivalent that also carries the
		// value type, so it is rendered as an object with one illustrative
		// entry. That is what protojson emits too, so the example stays valid.
		value := f.scalarOrMessage(fd.MapValue(), depth+1)

		return &apicatalog.Schema{
			Type:        "object",
			Description: "map<" + kindName(fd.MapKey()) + ", " + kindName(fd.MapValue()) + ">",
			Properties: []apicatalog.Property{{
				Name:   "<" + kindName(fd.MapKey()) + ">",
				Schema: value,
			}},
		}
	}

	inner := f.scalarOrMessage(fd, depth+1)

	if fd.IsList() {
		return &apicatalog.Schema{Type: "array", Items: inner}
	}

	return inner
}

func (f *flattener) scalarOrMessage(fd protoreflect.FieldDescriptor, depth int) *apicatalog.Schema {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return f.wellKnownOrMessage(fd.Message(), depth)

	case protoreflect.EnumKind:
		return enumSchema(fd.Enum())

	default:
		return &apicatalog.Schema{Type: jsonType(fd.Kind()), Format: jsonFormat(fd.Kind())}
	}
}

// wellKnownOrMessage renders the well-known types the way protojson does rather
// than as their structural definition.
//
// Without this, a Timestamp field documents as an object with seconds and nanos
// — which is what the wire carries, and precisely not what a JSON caller sends.
// The catalog exists to show what to send.
func (f *flattener) wellKnownOrMessage(md protoreflect.MessageDescriptor, depth int) *apicatalog.Schema {
	if md == nil {
		return &apicatalog.Schema{Type: "object"}
	}

	switch md.FullName() {
	case "google.protobuf.Timestamp":
		return &apicatalog.Schema{Type: "string", Format: "date-time"}
	case "google.protobuf.Duration":
		return &apicatalog.Schema{Type: "string", Description: "duration in seconds with up to 9 fractional digits, e.g. \"3.000000001s\""}
	case "google.protobuf.Empty":
		return &apicatalog.Schema{Type: "object"}
	case "google.protobuf.Struct", "google.protobuf.Value":
		return &apicatalog.Schema{Type: "object", Description: "arbitrary JSON"}
	case "google.protobuf.ListValue":
		return &apicatalog.Schema{Type: "array", Items: &apicatalog.Schema{Type: "object"}}
	case "google.protobuf.FieldMask":
		return &apicatalog.Schema{Type: "string", Description: "comma-separated field paths"}
	case "google.protobuf.Any":
		return &apicatalog.Schema{
			Type:        "object",
			Description: "any message; set @type to the fully-qualified type URL",
			Properties: []apicatalog.Property{
				{Name: "@type", Schema: &apicatalog.Schema{Type: "string"}},
			},
		}
	case "google.protobuf.BoolValue":
		return &apicatalog.Schema{Type: "boolean", Nullable: true}
	case "google.protobuf.StringValue":
		return &apicatalog.Schema{Type: "string", Nullable: true}
	case "google.protobuf.BytesValue":
		return &apicatalog.Schema{Type: "string", Format: "byte", Nullable: true}
	case "google.protobuf.Int32Value", "google.protobuf.UInt32Value":
		return &apicatalog.Schema{Type: "integer", Nullable: true}
	case "google.protobuf.Int64Value", "google.protobuf.UInt64Value":
		return &apicatalog.Schema{Type: "string", Format: "int64", Nullable: true}
	case "google.protobuf.FloatValue", "google.protobuf.DoubleValue":
		return &apicatalog.Schema{Type: "number", Nullable: true}
	}

	return f.message(md, depth)
}

func enumSchema(ed protoreflect.EnumDescriptor) *apicatalog.Schema {
	out := &apicatalog.Schema{Type: "string"}
	if ed == nil {
		return out
	}

	values := ed.Values()
	for i := range values.Len() {
		if len(out.Enum) >= 20 {
			break
		}
		out.Enum = append(out.Enum, string(values.Get(i).Name()))
	}

	return out
}

// jsonType maps a protobuf kind onto the JSON type protojson produces.
//
// The 64-bit integer kinds map to string, not integer: protojson encodes them
// as strings because IEEE-754 doubles cannot represent the full int64 range,
// and an example that shows a bare number would be rejected by the server.
func jsonType(kind protoreflect.Kind) string {
	switch kind {
	case protoreflect.BoolKind:
		return "boolean"

	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind,
		protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.Sint64Kind:
		return "integer"

	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "number"

	case protoreflect.Int64Kind, protoreflect.Uint64Kind,
		protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind:
		return "string"

	default:
		return "string"
	}
}

func jsonFormat(kind protoreflect.Kind) string {
	switch kind {
	case protoreflect.Int64Kind, protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind:
		return "int64"
	case protoreflect.Uint64Kind:
		return "uint64"
	case protoreflect.BytesKind:
		return "byte"
	default:
		return ""
	}
}

func kindName(fd protoreflect.FieldDescriptor) string {
	if fd == nil {
		return "value"
	}
	if fd.Kind() == protoreflect.MessageKind {
		return string(fd.Message().FullName())
	}

	return strings.ToLower(fd.Kind().String())
}
