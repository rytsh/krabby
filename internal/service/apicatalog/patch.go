package apicatalog

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/goccy/go-yaml"
)

// ApplyMergePatch applies an RFC 7386 JSON Merge Patch to a fetched API
// document and returns the result as JSON.
//
// The document may be JSON or YAML; it is normalized to JSON first, because a
// merge patch is defined over the JSON data model and because round-tripping
// YAML through a patch would otherwise discard the anchors, comments and key
// order that make it worth being YAML in the first place. Parsers accept JSON
// wherever they accept YAML, so normalizing costs nothing downstream.
//
// When patch is empty the document is returned untouched, still in its original
// form — the common case must not pay for a conversion it does not need.
func ApplyMergePatch(doc, patch json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(patch)) == 0 {
		return doc, nil
	}

	docJSON, err := toJSON(doc)
	if err != nil {
		return nil, err
	}

	var target any
	if err := json.Unmarshal(docJSON, &target); err != nil {
		return nil, fmt.Errorf("decode api document; %w", err)
	}

	var overlay any
	if err := json.Unmarshal(patch, &overlay); err != nil {
		return nil, fmt.Errorf("decode spec_patch; %w", err)
	}

	merged, err := json.Marshal(mergePatch(target, overlay))
	if err != nil {
		return nil, fmt.Errorf("encode patched api document; %w", err)
	}

	return merged, nil
}

// mergePatch is RFC 7386 section 2. A null value in the patch deletes the key;
// a non-object patch replaces the target outright.
func mergePatch(target, patch any) any {
	patchObj, ok := patch.(map[string]any)
	if !ok {
		return patch
	}

	targetObj, ok := target.(map[string]any)
	if !ok {
		targetObj = map[string]any{}
	}

	for key, value := range patchObj {
		if value == nil {
			delete(targetObj, key)

			continue
		}
		targetObj[key] = mergePatch(targetObj[key], value)
	}

	return targetObj
}

// toJSON normalizes a document that may be JSON or YAML into JSON.
//
// The discriminator is the first non-space byte rather than a content type,
// because the servers publishing these documents are unreliable about it: YAML
// specs are routinely served as application/json and JSON specs as text/plain.
// JSON is a subset of YAML 1.2, so a document that already starts with '{' is
// passed through untouched instead of being re-encoded.
func toJSON(doc json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(doc)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("api document is empty")
	}

	if trimmed[0] == '{' {
		return trimmed, nil
	}

	out, err := yaml.YAMLToJSON(trimmed)
	if err != nil {
		return nil, fmt.Errorf("convert api document from yaml; %w", err)
	}

	return out, nil
}
