// Package websource tracks non-git content sources (wiki pages, Confluence
// spaces, ...) as named collections. Each collection has a user-chosen name
// that becomes its search scope key ("web:<name>"), a type that selects the
// fetcher implementation, and a set of pages persisted as markdown files that
// feed the shared docs RAG index.
//
// Fetchers live in per-type subpackages (websource/confluence, websource/jira,
// websource/pages) and implement the Fetcher interface; new source types add
// a new subpackage and register their fetcher in the manager wiring.
package websource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"
)

// Collection types. Each type has a fetcher implementation in its own
// subpackage.
const (
	TypePages      = "pages"      // custom web: user-registered page URLs
	TypeConfluence = "confluence" // Confluence space via REST API
	TypeJira       = "jira"       // JIRA project / JQL query via REST API
)

// Collection status values.
const (
	StatusPending  = "pending"
	StatusFetching = "fetching"
	StatusReady    = "ready"
	StatusError    = "error"
)

// ScopePrefix namespaces web-source keys in the shared docs vector store.
// Repo ids can never contain ':' so the two key spaces cannot collide.
const ScopePrefix = "web:"

// ScopeKey returns the vector-store key of a collection ("web:<name>").
func ScopeKey(name string) string { return ScopePrefix + name }

// CollectionName returns the collection name of a scope key, or "" when the
// key is not a web-source key.
func CollectionName(scopeKey string) string {
	if !strings.HasPrefix(scopeKey, ScopePrefix) {
		return ""
	}

	return strings.TrimPrefix(scopeKey, ScopePrefix)
}

// Collection is one named web content source.
type Collection struct {
	// Name is the user-chosen identifier (e.g. "wine"). It is used in file
	// paths and as the search scope key, so it is restricted to
	// [a-z0-9][a-z0-9._-]*.
	Name string `bw:"name,pk" json:"name"`
	// Type selects the fetcher: TypePages, TypeConfluence or TypeJira.
	Type string `bw:"type" json:"type"`
	// Description is a short, human-written summary of what this source holds
	// (e.g. "Delivery Support runbooks and TERs"). It is surfaced to MCP
	// clients via list_sources so a model can pick the right source to search.
	Description string `bw:"description" json:"description,omitempty"`
	// RefreshInterval is how often the scheduler re-syncs the collection.
	// 0 disables automatic refresh (manual only).
	RefreshInterval time.Duration `bw:"refresh_interval" json:"refresh_interval"`

	Status        string    `bw:"status"     json:"status"`
	LastError     string    `bw:"last_error" json:"last_error,omitempty"`
	LastRefreshAt time.Time `bw:"last_refresh" json:"last_refresh_at,omitzero"`
	CreatedAt     time.Time `bw:"created_at" json:"created_at,omitzero"`

	// Config is opaque provider-owned JSON. The registered Fetcher validates,
	// merges and redacts it; the common model never needs a provider-specific
	// field when a new source type is added.
	Config json.RawMessage `bw:"config" json:"-"`

	// State is an opaque provider sync watermark (e.g. JIRA's last-updated
	// cursor for incremental fetches). It is written by the fetcher via
	// FetchResult.State and never exposed over the API.
	State json.RawMessage `bw:"state" json:"-"`
}

// Page is one synced document of a collection.
type Page struct {
	// ID is "<collection>/<slug>".
	ID         string `bw:"id,pk"            json:"id"`
	Collection string `bw:"collection,index" json:"collection"`
	// Slug is the markdown file name (without .md) inside the collection dir.
	Slug  string `bw:"slug"  json:"slug"`
	URL   string `bw:"url"   json:"url"`
	Title string `bw:"title" json:"title,omitempty"`
	// Teams are optional provider tags (e.g. JIRA team/squad values) used to
	// list and filter tickets by team.
	Teams []string `bw:"teams" json:"teams,omitempty"`
	// TeamsNorm holds the lowercase/trimmed form of every Teams value. It backs
	// case-insensitive team filtering at the store level (via the JSON "has
	// any" operator on this field), so listing a large source by team does not
	// load and scan the whole collection in memory. It is derived from Teams on
	// upsert and never set by callers.
	TeamsNorm []string `bw:"teams_norm,index" json:"-"`
	// Hash fingerprints the converted markdown so unchanged pages skip
	// re-embedding.
	Hash        string    `bw:"hash"       json:"-"`
	Status      string    `bw:"status"     json:"status"`
	LastError   string    `bw:"last_error" json:"last_error,omitempty"`
	LastFetchAt time.Time `bw:"last_fetch" json:"last_fetch_at,omitzero"`
}

// RemotePage is one fetched page, already converted to markdown.
type RemotePage struct {
	// Slug must be stable across fetches and unique within the collection.
	Slug     string
	Title    string
	URL      string
	Markdown string
	// Teams are optional provider-supplied tags (e.g. JIRA team/squad field
	// values) recorded on the page so tickets can be listed/filtered by team.
	Teams []string
	// Err marks a page that failed to fetch/convert; the sync records the
	// error on the page record and keeps the previous content.
	Err error
}

// FetchResult is the outcome of one fetch. Pages are the items returned this
// run. Incremental marks a partial fetch (only items changed since the last
// sync): the manager then upserts only those items and must NOT prune records
// it did not see, because unseen means unchanged, not deleted. Removed lists
// slugs the provider positively knows no longer match (so they can be pruned
// even in incremental mode). State is an opaque provider watermark the manager
// persists back onto the collection and hands to the next fetch.
type FetchResult struct {
	Pages       []RemotePage
	Incremental bool
	Removed     []string
	State       json.RawMessage
}

// Fetcher lists and converts the current remote pages of one collection.
// Implementations live in per-type subpackages. pages carries the persisted
// page records: URL-list types re-fetch them, discovery types (Confluence)
// may ignore them. state is the provider watermark returned by the previous
// fetch (nil on first run); providers that support incremental sync use it to
// fetch only what changed and return an advanced State.
type Fetcher interface {
	// Validate checks provider config before a collection is persisted.
	Validate(config json.RawMessage) error
	// MergeConfig merges an update with stored config. Providers implement
	// secret-preserving semantics here (blank write-only values keep existing).
	MergeConfig(current, update json.RawMessage) (json.RawMessage, error)
	// ConfigView returns a JSON-safe, redacted provider config for REST/UI.
	ConfigView(config json.RawMessage) any
	Fetch(ctx context.Context, col *Collection, pages []*Page, state json.RawMessage) (*FetchResult, error)
}

// nameRe restricts collection names to something safe for directories, URLS
// and scope keys.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidName reports whether name is a valid collection name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// schemaVersion must be bumped whenever Collection or Page change shape.
// v6: Page gained TeamsNorm (indexed, lowercase team tags) for store-level
// case-insensitive team filtering.
const schemaVersion = 6

// Store persists collections and pages.
type Store struct {
	collections *bw.Bucket[Collection]
	pages       *bw.Bucket[Page]
}

// New opens the web-source buckets on the given database.
func New(db *bw.DB) (*Store, error) {
	collections, err := bw.RegisterBucket[Collection](db, "web_collections",
		bw.WithVersion[Collection](schemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register web_collections bucket; %w", err)
	}

	pages, err := bw.RegisterBucket[Page](db, "web_pages",
		bw.WithVersion[Page](schemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register web_pages bucket; %w", err)
	}

	return &Store{collections: collections, pages: pages}, nil
}

// GetCollection returns a collection by name, or nil if it does not exist.
func (s *Store) GetCollection(ctx context.Context, name string) (*Collection, error) {
	col, err := s.collections.Get(ctx, name)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get collection %s; %w", name, err)
	}

	return col, nil
}

// ListCollections returns all collections sorted by name.
func (s *Store) ListCollections(ctx context.Context) ([]*Collection, error) {
	q, err := query.Parse("_limit=10000")
	if err != nil {
		return nil, fmt.Errorf("parse query; %w", err)
	}

	cols, err := s.collections.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list collections; %w", err)
	}

	if cols == nil {
		cols = []*Collection{}
	}

	sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })

	return cols, nil
}

// UpsertCollection inserts or replaces a collection record.
func (s *Store) UpsertCollection(ctx context.Context, col *Collection) error {
	if err := s.collections.Insert(ctx, col); err != nil {
		return fmt.Errorf("upsert collection %s; %w", col.Name, err)
	}

	return nil
}

// DeleteCollection removes a collection record and all its page records.
func (s *Store) DeleteCollection(ctx context.Context, name string) error {
	pages, err := s.Pages(ctx, name)
	if err != nil {
		return err
	}

	for _, p := range pages {
		if err := s.DeletePage(ctx, p.ID); err != nil {
			return err
		}
	}

	if err := s.collections.Delete(ctx, name); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete collection %s; %w", name, err)
	}

	return nil
}

// Pages returns the page records of one collection sorted by slug.
func (s *Store) Pages(ctx context.Context, collection string) ([]*Page, error) {
	q := query.New()
	q.Where = append(q.Where,
		query.NewExpressionCmp(query.OperatorEq, "collection", collection).Expression())
	q.SetLimit(100000)

	pages, err := s.pages.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list pages of %s; %w", collection, err)
	}

	if pages == nil {
		pages = []*Page{}
	}

	sort.Slice(pages, func(i, j int) bool { return pages[i].Slug < pages[j].Slug })

	return pages, nil
}

// pagesWhere builds the filter for one collection, optionally restricted to
// pages tagged with team (case-insensitive, matched against the normalized
// teams field via the JSON "has any" operator so the scan happens in the
// store, not in memory). An empty team matches the whole collection.
func pagesWhere(collection, team string) []query.Expression {
	where := []query.Expression{
		query.NewExpressionCmp(query.OperatorEq, "collection", collection).Expression(),
	}

	team = strings.ToLower(strings.TrimSpace(team))
	if team != "" {
		where = append(where,
			query.NewExpressionCmp(query.OperatorJIn, "teams_norm", []string{team}).Expression())
	}

	return where
}

// CountPages returns the number of page records in one collection, optionally
// restricted to a team (case-insensitive).
func (s *Store) CountPages(ctx context.Context, collection, team string) (int, error) {
	q := query.New()
	q.Where = pagesWhere(collection, team)

	n, err := s.pages.Count(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("count pages of %s; %w", collection, err)
	}

	return int(n), nil
}

// PagesPaged returns one page (offset/limit) of a collection's page records
// sorted by slug, together with the total count of matching records. When team
// is non-empty only pages tagged with that team (case-insensitive) are counted
// and returned. Filtering and paging happen at the store level so a large
// source (e.g. a Confluence sub-tree with thousands of pages) is not loaded
// into memory. offset < 0 is treated as 0; limit <= 0 returns no records (only
// the count).
func (s *Store) PagesPaged(ctx context.Context, collection, team string, offset, limit int) ([]*Page, int, error) {
	total, err := s.CountPages(ctx, collection, team)
	if err != nil {
		return nil, 0, err
	}

	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || offset >= total {
		return []*Page{}, total, nil
	}

	q := query.New()
	q.Where = pagesWhere(collection, team)
	q.Sort = []query.ExpressionSort{{Field: "slug"}}
	q.SetOffset(uint64(offset))
	q.SetLimit(uint64(limit))

	pages, err := s.pages.Find(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("list pages of %s; %w", collection, err)
	}

	if pages == nil {
		pages = []*Page{}
	}

	return pages, total, nil
}

// Teams returns the distinct team tags across one collection's pages, sorted,
// preserving the original casing of the first occurrence. Used to populate the
// UI team filter for jira sources. It scans the collection's page records, so
// callers should reserve it for small sources (jira), not large discovery
// sources.
func (s *Store) Teams(ctx context.Context, collection string) ([]string, error) {
	q := query.New()
	q.Where = append(q.Where,
		query.NewExpressionCmp(query.OperatorEq, "collection", collection).Expression())
	q.SetLimit(100000)

	pages, err := s.pages.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list pages of %s; %w", collection, err)
	}

	seen := map[string]string{} // lowercase -> original casing
	for _, p := range pages {
		for _, t := range p.Teams {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			key := strings.ToLower(t)
			if _, ok := seen[key]; !ok {
				seen[key] = t
			}
		}
	}

	out := make([]string, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})

	return out, nil
}

// GetPage returns a page by id, or nil if it does not exist.
func (s *Store) GetPage(ctx context.Context, id string) (*Page, error) {
	p, err := s.pages.Get(ctx, id)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get page %s; %w", id, err)
	}

	return p, nil
}

// UpsertPage inserts or replaces a page record. It derives TeamsNorm from Teams
// so team filtering can run at the store level, case-insensitively.
func (s *Store) UpsertPage(ctx context.Context, p *Page) error {
	p.TeamsNorm = normalizeTeams(p.Teams)

	if err := s.pages.Insert(ctx, p); err != nil {
		return fmt.Errorf("upsert page %s; %w", p.ID, err)
	}

	return nil
}

// normalizeTeams returns the lowercased, trimmed, de-duplicated team tags,
// dropping empties. Returns nil when there are no usable tags so the stored
// field stays absent for pages without teams.
func normalizeTeams(teams []string) []string {
	if len(teams) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(teams))
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// DeletePage removes a page record.
func (s *Store) DeletePage(ctx context.Context, id string) error {
	if err := s.pages.Delete(ctx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete page %s; %w", id, err)
	}

	return nil
}

// PageID builds the primary key of a page record.
func PageID(collection, slug string) string { return collection + "/" + slug }
