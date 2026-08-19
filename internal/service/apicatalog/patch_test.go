package apicatalog

import (
	"encoding/json"
	"strings"
	"testing"
)

const patchTestDoc = `{"openapi":"3.0.0","info":{"title":"Demo","version":"1.0.0"}}`

// A JSON null patch must leave the document alone.
//
// The UI's spec-patch box sends null when it is left empty, and RFC 7386 says a
// non-object patch replaces the target — so applying it literally turned the
// whole specification into null and the operator was told their document was
// unparseable ("spec type not supported by libopenapi, sorry"), with no hint
// that an empty optional field caused it.
func TestApplyMergePatchTreatsNullAsNoPatch(t *testing.T) {
	t.Parallel()

	for _, patch := range []string{``, `null`, `  null  `, "\n null \n"} {
		got, err := ApplyMergePatch(json.RawMessage(patchTestDoc), json.RawMessage(patch))
		if err != nil {
			t.Fatalf("ApplyMergePatch(%q) error = %v", patch, err)
		}
		if string(got) != patchTestDoc {
			t.Errorf("ApplyMergePatch(%q) = %s, want the document untouched", patch, got)
		}
	}
}

// The null exemption must not swallow patches that merely contain a null: a
// null *value* inside an object still deletes that key, which is the RFC 7386
// behaviour the feature exists for.
func TestApplyMergePatchStillDeletesKeysWithNullValues(t *testing.T) {
	t.Parallel()

	got, err := ApplyMergePatch(json.RawMessage(patchTestDoc), json.RawMessage(`{"info":null}`))
	if err != nil {
		t.Fatalf("ApplyMergePatch() error = %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if _, ok := out["info"]; ok {
		t.Errorf("info should have been deleted, got %s", got)
	}
	if out["openapi"] != "3.0.0" {
		t.Errorf("openapi key lost, got %s", got)
	}
}

// A quoted "null" is a string, not the null literal, and keeps the documented
// replace-the-target behaviour.
func TestApplyMergePatchKeepsScalarReplacement(t *testing.T) {
	t.Parallel()

	got, err := ApplyMergePatch(json.RawMessage(patchTestDoc), json.RawMessage(`"null"`))
	if err != nil {
		t.Fatalf("ApplyMergePatch() error = %v", err)
	}
	if strings.TrimSpace(string(got)) != `"null"` {
		t.Errorf("got %s, want the scalar to replace the document", got)
	}
}

func TestApplyMergePatchMergesObjects(t *testing.T) {
	t.Parallel()

	got, err := ApplyMergePatch(json.RawMessage(patchTestDoc), json.RawMessage(`{"info":{"title":"Renamed"}}`))
	if err != nil {
		t.Fatalf("ApplyMergePatch() error = %v", err)
	}

	var out struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if out.Info.Title != "Renamed" {
		t.Errorf("title = %q, want Renamed", out.Info.Title)
	}
	if out.Info.Version != "1.0.0" {
		t.Errorf("version = %q, want the untouched 1.0.0", out.Info.Version)
	}
	if out.OpenAPI != "3.0.0" {
		t.Errorf("openapi = %q, want 3.0.0", out.OpenAPI)
	}
}
