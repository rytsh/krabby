package websource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/rytsh/krabby/internal/service/repofs"
)

// KeyFilter selects remote keys (paths, object keys, URLs) with include and
// exclude globs using the same matcher as repository filters: "**" spans
// segments, a bare pattern also matches the base name, and a trailing "/"
// matches everything beneath a directory. Exclude wins.
type KeyFilter struct {
	Include []string
	Exclude []string
}

// Match reports whether key is selected.
func (f KeyFilter) Match(key string) bool {
	key = strings.TrimPrefix(key, "/")
	if repofs.MatchAny(f.Exclude, key) {
		return false
	}

	return len(f.Include) == 0 || repofs.MatchAny(f.Include, key)
}

// Signature fingerprints the filter for ListingSync.
func (f KeyFilter) Signature(extra ...string) string {
	parts := append([]string{strings.Join(f.Include, ","), strings.Join(f.Exclude, ",")}, extra...)

	return Hash(strings.Join(parts, "\x00"))
}

// Lookup reads a dotted path ("data.items", "fields.summary", "tags.0") from
// a decoded JSON value. An empty path returns v itself.
func Lookup(v any, path string) any {
	path = strings.TrimSpace(path)
	if path == "" || path == "." || path == "$" {
		return v
	}
	path = strings.TrimPrefix(strings.TrimPrefix(path, "$"), ".")
	for part := range strings.SplitSeq(path, ".") {
		switch cur := v.(type) {
		case map[string]any:
			v = cur[part]
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(cur) {
				return nil
			}
			v = cur[i]
		default:
			return nil
		}
	}

	return v
}

// Stringify renders a scalar JSON value as text; arrays of scalars are joined
// with ", " and objects are encoded as compact JSON.
func Stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case []byte:
		return string(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			if s := Stringify(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(raw)
	}
}

// StringList returns a value as a list of strings (a JSON array, or one
// scalar).
func StringList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				for _, k := range []string{"name", "value", "title", "label"} {
					if s := Stringify(m[k]); s != "" {
						out = append(out, s)
						break
					}
				}
				continue
			}
			if s := strings.TrimSpace(Stringify(e)); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		if s := strings.TrimSpace(Stringify(x)); s != "" {
			return []string{s}
		}
		return nil
	}
}

// ParseTime accepts the timestamp shapes APIs commonly return: RFC 3339 (with
// or without fractional seconds), "2006-01-02 15:04:05", a date, and Unix
// seconds or milliseconds.
func ParseTime(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case float64:
		return unixTime(int64(x))
	case int64:
		return unixTime(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return unixTime(n)
		}
	case string:
		s := strings.TrimSpace(x)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return unixTime(n)
		}
		for _, layout := range []string{
			time.RFC3339Nano, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700",
			"2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC1123Z, time.RFC1123, "2006-01-02",
		} {
			if t, err := time.Parse(layout, s); err == nil {
				return t
			}
		}
	}

	return time.Time{}
}

func unixTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n > 1e12 {
		return time.UnixMilli(n)
	}

	return time.Unix(n, 0)
}

var templateFuncs = template.FuncMap{
	"get":   Lookup,
	"str":   Stringify,
	"join":  func(sep string, v any) string { return strings.Join(StringList(v), sep) },
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"trim":  strings.TrimSpace,
	"json": func(v any) string {
		raw, _ := json.MarshalIndent(v, "", "  ")
		return string(raw)
	},
	"date": func(v any) string {
		if t := ParseTime(v); !t.IsZero() {
			return t.UTC().Format("2006-01-02 15:04")
		}
		return Stringify(v)
	},
	"default": func(def, v any) any {
		if Stringify(v) == "" {
			return def
		}
		return v
	},
}

// CompileTemplate parses a Go text/template used to render one record to
// Markdown. The record is the template's dot; helper functions get, str,
// join, lower, upper, trim, json, date and default are available.
func CompileTemplate(src string) (*template.Template, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	t, err := template.New("record").Funcs(templateFuncs).Option("missingkey=zero").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("invalid template; %w", err)
	}

	return t, nil
}

// ExpandTemplate renders a short inline template (URL patterns such as
// "https://tracker/{{.id}}") against a record.
func ExpandTemplate(src string, record any) (string, error) {
	t, err := CompileTemplate(src)
	if err != nil || t == nil {
		return "", err
	}

	return ExecTemplate(t, record)
}

// ExecTemplate runs a compiled template.
func ExecTemplate(t *template.Template, record any) (string, error) {
	var b bytes.Buffer
	if err := t.Execute(&b, record); err != nil {
		return "", fmt.Errorf("render template; %w", err)
	}

	return strings.TrimSpace(b.String()), nil
}

// RenderRecord renders a record without a template: an optional body field
// followed by a metadata list of the remaining scalar fields, so every value
// is still searchable.
func RenderRecord(record map[string]any, bodyKey string, skip ...string) string {
	var b strings.Builder
	skipSet := map[string]bool{bodyKey: true}
	for _, s := range skip {
		skipSet[s] = true
	}
	for _, k := range slices.Sorted(maps.Keys(record)) {
		if skipSet[k] {
			continue
		}
		v := record[k]
		if _, nested := v.(map[string]any); nested {
			raw, _ := json.Marshal(v)
			if len(raw) > 500 {
				continue
			}
		}
		s := strings.TrimSpace(Stringify(v))
		if s == "" {
			continue
		}
		if len(s) > 500 {
			s = s[:500] + "…"
		}
		fmt.Fprintf(&b, "- **%s**: %s\n", k, strings.ReplaceAll(s, "\n", " "))
	}
	if bodyKey != "" {
		if body := strings.TrimSpace(Stringify(record[bodyKey])); body != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(body)
			b.WriteString("\n")
		}
	}

	return b.String()
}
