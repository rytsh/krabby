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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"

	"github.com/rytsh/krabby/internal/service/vectorstore"
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

// ScopePrefix namespaces web-source keys in the shared docs index.
//
// The convention is owned by the store, which has to answer questions about
// it; this is a re-export so callers reading web-source code do not have to
// reach across packages for the spelling.
const ScopePrefix = vectorstore.ScopePrefix

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
	// AnalyzeImages opts this collection into the globally configured vision
	// pipeline. It is false by default so enabling a model never sends every
	// source's images without an explicit per-source choice.
	AnalyzeImages bool `bw:"analyze_images" json:"analyze_images"`
	// RefreshInterval is how often the scheduler re-syncs the collection.
	// 0 disables automatic refresh (manual only). Kept for backward
	// compatibility and as the fallback when Specs is empty: the scheduler then
	// polls the collection on an "@every <RefreshInterval>" cron.
	RefreshInterval time.Duration `bw:"refresh_interval" json:"refresh_interval"`
	// Specs are cron schedules (github.com/worldline-go/hardloop syntax, e.g.
	// "0 2 * * *" or "@every 6h") on which the scheduler re-syncs this
	// collection, mirroring per-namespace repository schedules. When non-empty
	// they are authoritative and RefreshInterval is ignored. An empty slice
	// falls back to RefreshInterval (or manual-only when that is 0 too).
	Specs []string `bw:"specs" json:"specs,omitempty"`

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

// EffectiveSpecs returns the cron schedules the scheduler should run for this
// collection: the explicit Specs when set, otherwise a single "@every
// <RefreshInterval>" derived from the legacy interval. It returns nil when the
// collection has neither (manual-only), so the scheduler skips it.
func (c Collection) EffectiveSpecs() []string {
	specs := make([]string, 0, len(c.Specs))
	for _, s := range c.Specs {
		if strings.TrimSpace(s) != "" {
			specs = append(specs, strings.TrimSpace(s))
		}
	}
	if len(specs) > 0 {
		return specs
	}

	if c.RefreshInterval > 0 {
		return []string{"@every " + c.RefreshInterval.String()}
	}

	return nil
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
	Hash string `bw:"hash"       json:"-"`
	// UpdatedAt is the source item's last-modified time (JIRA "updated",
	// Confluence version.when). Persisted so it can be re-applied to the page's
	// vectors during an index reconcile without re-fetching, and surfaced in
	// listings.
	UpdatedAt   time.Time `bw:"updated_at" json:"updated_at,omitzero"`
	Status      string    `bw:"status"     json:"status"`
	LastError   string    `bw:"last_error" json:"last_error,omitempty"`
	LastFetchAt time.Time `bw:"last_fetch" json:"last_fetch_at,omitzero"`
}

// ImageAnalysis caches one vision result. Raw image bytes and source URLs are
// deliberately absent: the cache is keyed by a hash of the reference and is
// valid only when both the downloaded content hash and analyzer engine match.
type ImageAnalysis struct {
	ID          string    `bw:"id,pk"`
	PageID      string    `bw:"page_id,index"`
	ContentHash string    `bw:"content_hash"`
	Engine      string    `bw:"engine"`
	Text        string    `bw:"text"`
	UpdatedAt   time.Time `bw:"updated_at"`
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
	// UpdatedAt is the source item's last-modified time (JIRA "updated",
	// Confluence version.when), when the provider supplies one. Recorded on the
	// page and its vectors so retrieval can surface recency and bias ranking.
	UpdatedAt time.Time
	// Err marks a page that failed to fetch/convert; the sync records the
	// error on the page record and keeps the previous content.
	Err error
}

// FetchResult is the outcome of one fetch. The pages themselves are streamed to
// the emit callback during the fetch rather than returned here, so a collection
// of any size costs the same memory.
//
// Complete is a guarantee about what was emitted, and it is the only thing that
// licenses deletion: it means every item the collection currently contains was
// emitted this run, so a stored record that was not seen is genuinely gone
// remotely. It must be false for an incremental fetch (unseen means unchanged,
// not deleted) and equally false for a full fetch that did not finish — one cut
// short by a cap, a provider limit or any other early exit. A truncated sweep
// that claims completeness makes the manager delete every record beyond the cut
// and re-embed it on the next sync.
//
// Removed lists slugs the provider positively knows no longer match; they are
// pruned regardless of Complete.
//
// State is an opaque provider watermark the manager persists back onto the
// collection and hands to the next fetch.
type FetchResult struct {
	Complete bool
	Removed  []string
	State    json.RawMessage
}

// Emit receives one fetched page. Returning an error aborts the fetch, which
// must propagate the error unchanged so the manager can tell a provider failure
// (retry later, do not advance the watermark) from a sink failure.
type Emit func(RemotePage) error

// Fetcher lists and converts the current remote pages of one collection.
// Implementations live in per-type subpackages. pages carries the persisted
// page records: URL-list types re-fetch them, discovery types (Confluence)
// may ignore them. state is the provider watermark returned by the previous
// fetch (nil on first run); providers that support incremental sync use it to
// fetch only what changed and return an advanced State.
//
// Pages are handed to emit as they are converted instead of being accumulated
// and returned. A provider must not buffer the whole collection: a space or
// project of any size then costs the same memory, which is what lets a full
// sweep run to completion instead of being capped — and only a sweep that
// completes may report Complete.
type Fetcher interface {
	// Validate checks provider config before a collection is persisted.
	Validate(config json.RawMessage) error
	// MergeConfig merges an update with stored config. Providers implement
	// secret-preserving semantics here (blank write-only values keep existing).
	MergeConfig(current, update json.RawMessage) (json.RawMessage, error)
	// ConfigView returns a JSON-safe, redacted provider config for REST/UI.
	ConfigView(config json.RawMessage) any
	Fetch(ctx context.Context, col *Collection, pages []*Page, state json.RawMessage, emit Emit) (*FetchResult, error)
}

// PreviewResult summarizes a read-only provider query before a collection is
// saved. ItemCount is the number of records left after provider-specific
// filters; Scanned is the number of remote candidates inspected. Total is set
// when the provider publishes the full result size (JIRA does, Confluence does
// not). Truncated reports that a configured sync limit stopped the walk early.
type PreviewResult struct {
	ItemCount int  `json:"item_count"`
	Scanned   int  `json:"scanned"`
	Total     int  `json:"total,omitempty"`
	Limit     int  `json:"limit,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

// Previewer is an optional provider capability for validating unsaved config
// and counting its scope without fetching content, persisting pages or indexing.
type Previewer interface {
	Preview(ctx context.Context, config json.RawMessage) (PreviewResult, error)
}

// FullResyncScheduler is implemented by incremental providers that need an
// occasional complete reconciliation. The manager adds this cron spec to the
// collection's regular auto-refresh specs, so the full-sync time itself wakes
// the source instead of waiting for a later incremental run.
type FullResyncScheduler interface {
	FullResyncSpec(config json.RawMessage) (string, error)
}

// SitemapFetcher is implemented by source types that can discover page URLs
// from a sitemap. It stays separate from Fetcher because discovery is only
// meaningful for user-managed URL collections.
type SitemapFetcher interface {
	SitemapURLs(ctx context.Context, sitemapURL string) ([]string, error)
}

// nameRe restricts collection names to something safe for directories, URLS
// and scope keys.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidName reports whether name is a valid collection name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// schemaVersion must be bumped whenever Collection or Page change shape.
// v6: Page gained TeamsNorm (indexed, lowercase team tags) for store-level
// case-insensitive team filtering.
// v7: Collection gained Specs (per-source cron schedules).
// v8: Page gained UpdatedAt (source last-modified time for recency).
// v9: Collection gained AnalyzeImages (per-source vision opt-in).
const schemaVersion = 9

// Store persists collections and pages.
type Store struct {
	collections *bw.Bucket[Collection]
	pages       *bw.Bucket[Page]
	images      *bw.Bucket[ImageAnalysis]
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
	images, err := bw.RegisterBucket[ImageAnalysis](db, "web_image_analysis", bw.WithVersion[ImageAnalysis](1))
	if err != nil {
		return nil, fmt.Errorf("register web image analysis bucket; %w", err)
	}

	return &Store{collections: collections, pages: pages, images: images}, nil
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
	q, err := query.Parse("_limit=100000")
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
func pagesWhere(collection, team, titleQuery string) []query.Expression {
	where := []query.Expression{
		query.NewExpressionCmp(query.OperatorEq, "collection", collection).Expression(),
	}

	team = strings.ToLower(strings.TrimSpace(team))
	if team != "" {
		where = append(where,
			query.NewExpressionCmp(query.OperatorJIn, "teams_norm", []string{team}).Expression())
	}
	titleQuery = strings.TrimSpace(titleQuery)
	if titleQuery != "" {
		where = append(where,
			query.NewExpressionCmp(query.OperatorILike, "title", "%"+titleQuery+"%").Expression())
	}

	return where
}

// CountPages returns the number of page records in one collection, optionally
// restricted to a team (case-insensitive).
func (s *Store) CountPages(ctx context.Context, collection, team string) (int, error) {
	q := query.New()
	q.Where = pagesWhere(collection, team, "")

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
func (s *Store) PagesPaged(ctx context.Context, collection, team, titleQuery string, offset, limit int) ([]*Page, int, error) {
	countQuery := query.New()
	countQuery.Where = pagesWhere(collection, team, titleQuery)
	count, err := s.pages.Count(ctx, countQuery)
	if err != nil {
		return nil, 0, fmt.Errorf("count pages of %s; %w", collection, err)
	}
	total := int(count)

	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || offset >= total {
		return []*Page{}, total, nil
	}

	q := query.New()
	q.Where = pagesWhere(collection, team, titleQuery)
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
	if err := s.DeleteImageAnalyses(ctx, id); err != nil {
		return err
	}
	if err := s.pages.Delete(ctx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete page %s; %w", id, err)
	}

	return nil
}

// ImageAnalysisID builds a stable cache key from image content, so rotating
// signed URLs neither leak into storage nor create duplicate cache rows.
func ImageAnalysisID(pageID, contentHash string) string {
	return pageID + "/" + contentHash
}

func (s *Store) GetImageAnalysis(ctx context.Context, id string) (*ImageAnalysis, error) {
	rec, err := s.images.Get(ctx, id)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get image analysis %s; %w", id, err)
	}
	return rec, nil
}

func (s *Store) UpsertImageAnalysis(ctx context.Context, rec *ImageAnalysis) error {
	if err := s.images.Insert(ctx, rec); err != nil {
		return fmt.Errorf("upsert image analysis %s; %w", rec.ID, err)
	}
	return nil
}

func (s *Store) DeleteImageAnalyses(ctx context.Context, pageID string) error {
	q := query.New()
	q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "page_id", pageID).Expression())
	q.SetLimit(10000)
	records, err := s.images.Find(ctx, q)
	if err != nil {
		return fmt.Errorf("list image analyses for %s; %w", pageID, err)
	}
	for _, rec := range records {
		if err := s.images.Delete(ctx, rec.ID); err != nil && !errors.Is(err, bw.ErrNotFound) {
			return fmt.Errorf("delete image analysis %s; %w", rec.ID, err)
		}
	}
	return nil
}

// PageID builds the primary key of a page record.
func PageID(collection, slug string) string { return collection + "/" + slug }

// slugRe is what a slug may contain. It is deliberately narrower than what
// Slugify produces so that a slug is always a single, self-contained path
// element: no separator, no dot segment, no leading dash.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidSlug reports whether a slug is safe to use as a page identity and as a
// filename.
//
// A slug reaches the filesystem as "<slug>.md" under the collection's directory
// and reaches the store as "<collection>/<slug>", and it arrives from two
// untrusted directions: a remote provider's response (a JIRA issue key, a
// Confluence page id) and a REST query parameter. filepath.Join resolves ".."
// instead of rejecting it, so an unchecked slug turns a page delete into a
// delete of any *.md file the process can reach, and a "/" in a slug makes the
// store key ambiguous. Slugify is safe by construction, but not every provider
// runs its identifiers through it — so the guard lives here, at the point where
// the slug is trusted, rather than at each producer.
func ValidSlug(slug string) bool {
	return slug != "" && len(slug) <= 200 && slug != "." && slug != ".." && slugRe.MatchString(slug)
}

// PageFile returns the path of a slug's markdown inside dir, refusing any slug
// that is not a safe, single path element. Every read, write and delete of a
// page's markdown must go through it.
func PageFile(dir, slug string) (string, error) {
	if !ValidSlug(slug) {
		return "", fmt.Errorf("unsafe page slug %q", slug)
	}

	return filepath.Join(dir, slug+".md"), nil
}
