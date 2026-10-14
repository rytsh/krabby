package websource

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rytsh/krabby/internal/service/progress"
)

// ListedItem is one remote entry of a listing-based provider (file systems,
// object stores, drives, feeds). Version is any change token the remote
// publishes (ETag, mtime+size, revision id); an unchanged Version lets a sync
// skip downloading and converting the item.
type ListedItem struct {
	// Key is the stable remote identity (object key, file path, item id).
	Key string
	// Path is the human-readable location shown in the document; it defaults
	// to Key (providers keyed by opaque ids set it to the file path).
	Path      string
	Version   string
	Title     string
	URL       string
	UpdatedAt time.Time
	Teams     []string
}

// listingState is the persisted watermark of a ListingSync: the version and
// slug of every key emitted by the previous successful sync.
type listingState struct {
	Items  map[string]listingEntry `json:"items"`
	Filter string                  `json:"filter,omitempty"`
}

type listingEntry struct {
	Version string `json:"v"`
	Slug    string `json:"s"`
}

// ListingSlug derives a stable, valid slug from a remote key.
func ListingSlug(key string) string {
	return Slugify(key) + "-" + Hash(key)[:10]
}

// ListingSync runs the incremental protocol shared by listing-based providers.
//
// Every listed item whose Version differs from the previous run is loaded and
// emitted; unchanged items are skipped. Keys present last time but absent now
// are reported in Removed, so deletions are reconciled even on incremental
// runs. A changed filter signature (or a forced full sync, which arrives as a
// nil state) re-emits every item, and only such a run reports Complete.
//
// items must be the full current listing: a provider that stopped listing
// early (a cap) must not call ListingSync with the partial list, or the keys
// past the cut would be reported as removed.
//
// load returns the converted page for one item; a load error is recorded on
// that page (RemotePage.Err) and the item keeps its previous version so the
// next sync retries it.
func ListingSync(
	ctx context.Context,
	rawState json.RawMessage,
	filter string,
	items []ListedItem,
	load func(ctx context.Context, item ListedItem) (RemotePage, error),
	emit Emit,
) (*FetchResult, error) {
	var prev listingState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &prev)
	}
	// A changed filter re-emits everything; the old entries are still used to
	// report keys that dropped out of the selection.
	reloadAll := prev.Filter != filter

	next := listingState{Items: make(map[string]listingEntry, len(items)), Filter: filter}
	seen := make(map[string]bool, len(items))
	emittedAll := true

	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seen[item.Key] = true
		slug := ListingSlug(item.Key)
		old, had := prev.Items[item.Key]

		if !reloadAll && had && old.Version != "" && old.Version == item.Version {
			next.Items[item.Key] = old
			emittedAll = false
			progress.Report(ctx, i+1, len(items))

			continue
		}

		page, err := load(ctx, item)
		if err != nil {
			page = RemotePage{Err: err}
		}
		page.Slug = slug
		if page.Title == "" {
			page.Title = item.Title
		}
		if page.URL == "" {
			page.URL = item.URL
		}
		if page.UpdatedAt.IsZero() {
			page.UpdatedAt = item.UpdatedAt
		}
		if page.Teams == nil {
			page.Teams = item.Teams
		}

		if err := emit(page); err != nil {
			return nil, err
		}

		if page.Err == nil {
			next.Items[item.Key] = listingEntry{Version: item.Version, Slug: slug}
		} else if had {
			// Keep the old entry with a blank version: the page stays (it is
			// not removed) and the next run retries it.
			next.Items[item.Key] = listingEntry{Slug: old.Slug}
		}
		progress.Report(ctx, i+1, len(items))
	}

	var removed []string
	for key, entry := range prev.Items {
		if !seen[key] {
			removed = append(removed, entry.Slug)
		}
	}

	state, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode listing state; %w", err)
	}

	// When every listed item was emitted (first run, forced full sync, filter
	// change) the sweep is complete and the manager may prune anything else.
	return &FetchResult{Complete: emittedAll, Removed: removed, State: state}, nil
}
