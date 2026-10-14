package websource

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Field kinds understood by the generic config form in the UI and by the
// schema-driven merge/redaction helpers.
const (
	FieldString = "string"
	FieldText   = "text"   // multi-line string
	FieldSecret = "secret" // write-only string; redacted to <key>_set
	FieldNumber = "number"
	FieldBool   = "bool"
	FieldList   = "list"   // []string, comma separated in the UI
	FieldSelect = "select" // string restricted to Options
	FieldMap    = "map"    // map[string]string; values are write-only
)

// Field describes one provider config property.
type Field struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Kind        string   `json:"kind"`
	Required    bool     `json:"required,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Options     []string `json:"options,omitempty"`
	Default     any      `json:"default,omitempty"`
}

// Descriptor documents one source type for the UI form and for MCP clients.
type Descriptor struct {
	Type        string  `json:"type"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields,omitempty"`
	// Preview reports that the type implements Previewer ("Test & preview").
	Preview bool `json:"preview,omitempty"`
	// ManualItems reports that items are pushed by clients (push type) or
	// added by users (pages type) rather than discovered by the provider.
	ManualItems bool `json:"manual_items,omitempty"`
}

// Describer is implemented by fetchers that publish a Descriptor.
type Describer interface {
	Describe() Descriptor
}

// Schema implements the Fetcher config plumbing (merge, redaction) for a
// provider whose config is a flat JSON object described by Fields. Providers
// embed it and add their own Validate.
//
// Merge rules match the hand-written providers: a key absent from the update
// keeps the stored value, an explicit null clears it, and a value overrides it.
// Secret fields and map values are write-only, so a blank value keeps the
// stored secret.
type Schema struct {
	Descriptor
}

func (s Schema) field(key string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Key == key {
			return f, true
		}
	}

	return Field{}, false
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("config must be a JSON object; %w", err)
	}

	return out, nil
}

// MergeObjects merges update onto current following the Schema rules and
// rejects unknown keys so a typo does not silently do nothing.
func (s Schema) MergeObjects(current, update json.RawMessage) (json.RawMessage, error) {
	cur, err := decodeObject(current)
	if err != nil {
		return nil, err
	}
	next, err := decodeObject(update)
	if err != nil {
		return nil, err
	}

	for key, value := range next {
		f, ok := s.field(key)
		if !ok {
			// Redacted view keys may be echoed back by clients; ignore them.
			if strings.HasSuffix(key, "_set") || strings.HasSuffix(key, "_names") {
				continue
			}

			return nil, fmt.Errorf("%s config: unknown field %q", s.Type, key)
		}
		if string(value) == "null" {
			delete(cur, key)

			continue
		}

		switch f.Kind {
		case FieldSecret:
			var v string
			if err := json.Unmarshal(value, &v); err != nil {
				return nil, fmt.Errorf("%s config: %s must be a string", s.Type, key)
			}
			if v == "" {
				continue // blank keeps the stored secret
			}
		case FieldMap:
			merged, err := mergeSecretMap(cur[key], value)
			if err != nil {
				return nil, fmt.Errorf("%s config: %s; %w", s.Type, key, err)
			}
			value = merged
		}

		cur[key] = value
	}

	return json.Marshal(cur)
}

// mergeSecretMap merges a header-like map whose values are write-only: a blank
// value keeps the stored value of the same key, and keys absent from the update
// are dropped (the update names the full key set).
func mergeSecretMap(current, update json.RawMessage) (json.RawMessage, error) {
	var cur, next map[string]string
	if len(current) != 0 {
		_ = json.Unmarshal(current, &cur)
	}
	if err := json.Unmarshal(update, &next); err != nil {
		return nil, fmt.Errorf("must be an object of strings")
	}
	out := make(map[string]string, len(next))
	for k, v := range next {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if v == "" {
			v = cur[k]
		}
		if v == "" {
			return nil, fmt.Errorf("new key %q needs a value", k)
		}
		out[k] = v
	}

	return json.Marshal(out)
}

// View returns the config as a JSON-safe object with secrets replaced by
// "<key>_set" booleans and map values replaced by "<key>_names".
func (s Schema) View(raw json.RawMessage) any {
	obj, err := decodeObject(raw)
	if err != nil {
		return nil
	}
	out := make(map[string]any, len(obj))
	for _, f := range s.Fields {
		value, ok := obj[f.Key]
		switch f.Kind {
		case FieldSecret:
			var v string
			if ok {
				_ = json.Unmarshal(value, &v)
			}
			out[f.Key+"_set"] = v != ""
		case FieldMap:
			var m map[string]string
			if ok {
				_ = json.Unmarshal(value, &m)
			}
			out[f.Key+"_names"] = slices.Sorted(maps.Keys(m))
		default:
			if ok {
				out[f.Key] = value
			}
		}
	}

	return out
}

// Describe returns the descriptor.
func (s Schema) Describe() Descriptor { return s.Descriptor }

// DecodeConfig unmarshals a provider config into its typed form.
func DecodeConfig[T any](raw json.RawMessage, typ string) (T, error) {
	var cfg T
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("decode %s config; %w", typ, err)
	}

	return cfg, nil
}
