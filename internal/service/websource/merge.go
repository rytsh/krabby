package websource

import (
	"fmt"
	"strings"
	"time"

	"github.com/worldline-go/types"
)

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
	Description   types.Null[string] `json:"description"`
	AnalyzeImages types.Null[bool]   `json:"analyze_images"`

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
	if u.Description.Present() {
		col.Description = strings.TrimSpace(u.Description.ValueOrZero())
	}
	if u.AnalyzeImages.Present() {
		col.AnalyzeImages = u.AnalyzeImages.ValueOrZero()
	}

	if u.Specs.Present() {
		specs := make([]string, 0, len(u.Specs.ValueOrZero()))
		for _, spec := range u.Specs.ValueOrZero() {
			if spec = strings.TrimSpace(spec); spec != "" {
				specs = append(specs, spec)
			}
		}
		col.Specs = specs
	}

	if u.RefreshInterval.Present() {
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
