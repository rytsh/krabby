// Package push implements the "push" web-source type: Krabby fetches nothing,
// and clients (CI pipelines, scripts, internal tools, MCP agents) write items
// into the collection through the REST API or MCP. Each item is identified by
// a client-chosen external id, so re-pushing the same id updates it in place.
package push

import (
	"context"
	"encoding/json"

	"github.com/rytsh/krabby/internal/service/websource"
)

// Type is the collection type served by this fetcher.
const Type = websource.TypePush

// Fetcher is the no-op fetcher of push collections.
type Fetcher struct {
	websource.Schema
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "Push (API / CI ingest)",
		Description: "Krabby does not fetch anything: clients push Markdown, HTML, text or binary documents " +
			"(PDF, DOCX, …) with an external id through POST /api/v1/sources/{name}/items or the " +
			"push_source_items MCP tool. Re-pushing an id updates it; prune removes ids a full export no longer contains.",
		ManualItems: true,
	}}}
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	_, err := f.MergeObjects(nil, raw)

	return err
}

func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	return f.MergeObjects(current, update)
}

func (f *Fetcher) ConfigView(raw json.RawMessage) any { return f.View(raw) }

// Fetch emits nothing: the stored items are authoritative. The sync that runs
// after it still reconciles the search index against the stored items.
func (f *Fetcher) Fetch(context.Context, *websource.Collection, []*websource.Page, json.RawMessage, websource.Emit) (*websource.FetchResult, error) {
	return &websource.FetchResult{}, nil
}
