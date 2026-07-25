package websource

import (
	"fmt"
	"strings"
	"time"

	"github.com/worldline-go/types"
)

// MergeNull returns the update value when the field was present in the update
// JSON (set to a value OR explicitly null), otherwise the stored value.
//
// It implements krabby's partial-update rule — absent = keep, null = clear,
// value = override — on top of types.Null's ParsedNull marker, which is the one
// thing a plain Go zero value cannot express: whether the client said "leave
// this alone" or "make this empty".
//
// It lives here rather than in each provider because every partial-update
// surface (provider configs, the source envelope, settings) has to agree on
// what an omitted field means.
func MergeNull[T any](update, stored types.Null[T]) types.Null[T] {
	if update.Valid || update.ParsedNull {
		return update
	}

	return stored
}

// Present reports whether the field was in the update JSON at all, with either
// a value or an explicit null. Use it when a merge is not a straight
// replacement — a secret that is kept unless explicitly cleared, say.
func Present[T any](n types.Null[T]) bool {
	return n.Valid || n.ParsedNull
}

// CollectionUpdate is a partial update of a collection's mutable envelope: the
// fields that live on the record itself rather than in the provider-owned
// Config blob.
//
// It exists because those two halves used to disagree. A provider config merged
// per field (absent = keep), while the envelope around it was replaced
// wholesale, so a request that changed only the description also silently wiped
// the collection's cron schedule and refresh interval. Every field here now
// follows the same rule as the config: absent = keep, null = clear, value =
// override.
//
// Name is not part of the payload: it identifies the collection and comes from
// the route. Type is immutable once created.
type CollectionUpdate struct {
	Description types.Null[string] `json:"description"`

	// RefreshInterval is a Go duration string ("24h"). Null or empty means
	// manual only. It is ignored while Specs is non-empty.
	RefreshInterval types.Null[string] `json:"refresh_interval"`

	// Specs are cron schedules (hardloop syntax). Null or an empty list falls
	// back to RefreshInterval.
	Specs types.Null[[]string] `json:"specs"`
}

// Apply overlays the update onto a stored collection, leaving fields the client
// did not mention untouched. The provider config is merged separately by the
// fetcher, which owns its shape.
func (u CollectionUpdate) Apply(col *Collection) error {
	if Present(u.Description) {
		col.Description = strings.TrimSpace(u.Description.ValueOrZero())
	}

	if Present(u.Specs) {
		specs := make([]string, 0, len(u.Specs.ValueOrZero()))
		for _, spec := range u.Specs.ValueOrZero() {
			if spec = strings.TrimSpace(spec); spec != "" {
				specs = append(specs, spec)
			}
		}
		col.Specs = specs
	}

	if Present(u.RefreshInterval) {
		raw := strings.TrimSpace(u.RefreshInterval.ValueOrZero())
		switch raw {
		case "", "manual":
			col.RefreshInterval = 0
		default:
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("refresh_interval; %w", err)
			}
			col.RefreshInterval = d
		}
	}

	return nil
}
