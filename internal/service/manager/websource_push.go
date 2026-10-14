package manager

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rytsh/krabby/internal/service/websource"
)

// pushSlug maps a client-chosen push item id to its page slug. The id is
// hashed in so ids differing only in punctuation never collide.
func pushSlug(id string) string { return "item-" + websource.ListingSlug(id) }

// PushSlug exposes the id→slug mapping to REST/MCP callers that delete by id.
func PushSlug(id string) string { return pushSlug(strings.TrimSpace(id)) }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}

// decodeImportContent returns the raw bytes of an imported item.
func decodeImportContent(item WebPageImport) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(item.Encoding)) {
	case "", "utf-8", "utf8", "text":
		return []byte(item.Content), nil
	case "base64":
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(item.Content))
		if err != nil {
			return nil, fmt.Errorf("content is not valid base64; %w", err)
		}

		return data, nil
	default:
		return nil, fmt.Errorf("unsupported encoding %q (want base64 or empty)", item.Encoding)
	}
}

// PrunePushItems deletes every item of a push collection whose id is not in
// keep. CI jobs that push a complete export call it once after the batches,
// so items removed upstream disappear from the index too. It returns the
// number of items deleted.
func (m *Manager) PrunePushItems(ctx context.Context, name string, keep []string) (int, error) {
	if m.webStore == nil {
		return 0, ErrNoWebSources
	}
	scope := websource.ScopeKey(name)
	defer m.lockKey(scope)()

	col, err := m.webStore.GetCollection(ctx, name)
	if err != nil {
		return 0, err
	}
	if col == nil {
		return 0, fmt.Errorf("collection %s not found", name)
	}
	if col.Type != websource.TypePush {
		return 0, fmt.Errorf("collection %s is type %q; prune requires type %q", name, col.Type, websource.TypePush)
	}

	keepSlugs := make(map[string]bool, len(keep))
	for _, id := range keep {
		if id = strings.TrimSpace(id); id != "" {
			keepSlugs[pushSlug(id)] = true
		}
	}
	if len(keepSlugs) == 0 {
		return 0, errors.New("keep must list at least one id; delete the source to drop everything")
	}

	pages, err := m.webStore.Pages(ctx, name)
	if err != nil {
		return 0, err
	}
	dir := m.sourcesDir(name)
	var removed []string
	for _, p := range pages {
		if keepSlugs[p.Slug] {
			continue
		}
		if err := m.webStore.DeletePage(ctx, p.ID); err != nil {
			return len(removed), err
		}
		if file, err := websource.PageFile(dir, p.Slug); err == nil {
			_ = os.Remove(file)
			removed = append(removed, docFileName(p.Slug))
		}
	}
	if len(removed) > 0 {
		if err := m.indexPaths(ctx, m.webCorpus(name), nil, removed, nil); err != nil {
			return len(removed), fmt.Errorf("drop pruned items from index; %w", err)
		}
	}

	return len(removed), nil
}
