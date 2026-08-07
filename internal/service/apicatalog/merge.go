package apicatalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/nullx"
)

// maxSpecPatchBytes bounds a stored merge patch. A patch is hand-written
// correction, not a second copy of the document: anything approaching this size
// means the spec should be fixed at its source or served from somewhere else.
const maxSpecPatchBytes = 256 << 10

// maxOperationOverrides bounds the per-operation override map. The map is read
// on every operation of every sync, so an unbounded one turns a linear walk
// into a memory problem for no benefit — a spec needing hundreds of individual
// corrections wants a SpecPatch.
const maxOperationOverrides = 500

// ServiceUpdate is a partial update of a service's mutable envelope: everything
// that lives on the record rather than in the provider-owned Config blob.
//
// Every field follows the same rule as the config — absent = keep, null =
// clear, value = override — so a request that changes only the description
// cannot silently wipe the cron schedule or the base-URL override.
//
// Name is not part of the payload: it identifies the service and comes from the
// route. Kind is immutable once created, because changing it would reinterpret
// every stored operation.
type ServiceUpdate struct {
	Group       types.Null[string] `json:"group"`
	Description types.Null[string] `json:"description"`

	// BaseURL replaces the servers declared by the document. Null or empty
	// falls back to what the spec says.
	BaseURL types.Null[string] `json:"base_url"`

	// SpecPatch is an RFC 7386 JSON Merge Patch applied to the raw document
	// before parsing. Null clears it.
	SpecPatch types.Null[json.RawMessage] `json:"spec_patch"`

	// Operations replaces the per-operation override map wholesale. It is not
	// merged key by key: an override map is small and hand-maintained, and a
	// per-key merge would leave no way to remove an entry short of a second
	// null-valued convention that callers would have to learn.
	Operations types.Null[map[string]OperationOverride] `json:"operations"`

	// RefreshInterval is a Go duration string ("24h"). Null or empty means
	// manual only. Ignored while Specs is non-empty.
	RefreshInterval types.Null[string] `json:"refresh_interval"`

	// Specs are cron schedules (hardloop syntax). Null or an empty list falls
	// back to RefreshInterval.
	Specs types.Null[[]string] `json:"specs"`
}

// Apply overlays the update onto a stored service, leaving fields the client
// did not mention untouched. The provider config is merged separately by the
// provider, which owns its shape.
//
// It reports whether the change invalidates the stored operations. A new
// SpecPatch, base URL or override map changes what every operation renders to,
// so the caller must force a full re-sync rather than wait for the watermark to
// move — the document itself has not changed, only our reading of it.
func (u ServiceUpdate) Apply(svc *Service) (rerender bool, err error) {
	if nullx.Present(u.Group) {
		group := NormalizeGroup(u.Group.ValueOrZero())
		if group != "" && !ValidName(group) {
			return false, fmt.Errorf("invalid group name %q (want lowercase [a-z0-9._-])", group)
		}
		svc.Group = group
	}
	if nullx.Present(u.Description) {
		svc.Description = strings.TrimSpace(u.Description.ValueOrZero())
	}

	if nullx.Present(u.BaseURL) {
		base := strings.TrimRight(strings.TrimSpace(u.BaseURL.ValueOrZero()), "/")
		if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return false, fmt.Errorf("base_url must be an http(s) URL")
		}
		if base != svc.BaseURL {
			rerender = true
		}
		svc.BaseURL = base
	}

	if nullx.Present(u.SpecPatch) {
		patch, err := normalizeSpecPatch(u.SpecPatch.ValueOrZero())
		if err != nil {
			return false, err
		}
		if !jsonEqual(patch, svc.SpecPatch) {
			rerender = true
		}
		svc.SpecPatch = patch
	}

	if nullx.Present(u.Operations) {
		overrides, err := normalizeOverrides(u.Operations.ValueOrZero())
		if err != nil {
			return false, err
		}
		rerender = true
		svc.Operations = overrides
	}

	if nullx.Present(u.Specs) {
		specs := make([]string, 0, len(u.Specs.ValueOrZero()))
		for _, spec := range u.Specs.ValueOrZero() {
			if spec = strings.TrimSpace(spec); spec != "" {
				specs = append(specs, spec)
			}
		}
		svc.Specs = specs
	}

	if nullx.Present(u.RefreshInterval) {
		raw := strings.TrimSpace(u.RefreshInterval.ValueOrZero())
		switch raw {
		case "", "manual":
			svc.RefreshInterval = 0
		default:
			d, err := time.ParseDuration(raw)
			if err != nil {
				return false, fmt.Errorf("refresh_interval; %w", err)
			}
			svc.RefreshInterval = d
		}
	}

	return rerender, nil
}

// normalizeSpecPatch validates a merge patch and returns its canonical stored
// form, or nil when the patch is empty.
//
// The patch must be a JSON object because RFC 7386 defines merging only over
// objects: a scalar or array patch means "replace the whole document", which is
// never what a user editing a spec override intends, and would silently discard
// the fetched document.
func normalizeSpecPatch(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, nil
	}

	if len(trimmed) > maxSpecPatchBytes {
		return nil, fmt.Errorf("spec_patch is %d bytes, limit %d", len(trimmed), maxSpecPatchBytes)
	}

	var probe map[string]any
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, fmt.Errorf("spec_patch must be a JSON object; %w", err)
	}

	// Re-marshal so the stored form is canonical and free of the caller's
	// whitespace; a patch is compared against its predecessor to decide whether
	// operations need re-rendering, and formatting is not a change.
	out, err := json.Marshal(probe)
	if err != nil {
		return nil, fmt.Errorf("encode spec_patch; %w", err)
	}

	return out, nil
}

// normalizeOverrides trims and validates the per-operation override map,
// dropping entries that would have no effect.
func normalizeOverrides(in map[string]OperationOverride) (map[string]OperationOverride, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxOperationOverrides {
		return nil, fmt.Errorf("operations holds %d overrides, limit %d", len(in), maxOperationOverrides)
	}

	out := make(map[string]OperationOverride, len(in))
	for key, ov := range in {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("operations has an entry with an empty operation id")
		}

		ov.Summary = strings.TrimSpace(ov.Summary)
		ov.Description = strings.TrimSpace(ov.Description)

		tags := make([]string, 0, len(ov.Tags))
		for _, t := range ov.Tags {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
		ov.Tags = tags

		if ov.Summary == "" && ov.Description == "" && len(ov.Tags) == 0 && !ov.Hidden {
			continue // nothing to apply
		}

		out[key] = ov
	}

	if len(out) == 0 {
		return nil, nil
	}

	return out, nil
}

// jsonEqual compares two raw JSON documents by value, treating nil and empty as
// equal.
func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}

	return string(a) == string(b)
}

// Override returns the stored override for an operation id, if any.
func (s Service) Override(operationID string) (OperationOverride, bool) {
	ov, ok := s.Operations[operationID]

	return ov, ok
}
