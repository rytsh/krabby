// Package confluence implements the Confluence web-source fetcher: it lists
// pages of a space, or of one page's sub-tree (root_page), through the
// Confluence REST API, filters by labels and converts the storage-format HTML
// to markdown. Listing uses CQL so syncs are incremental: after the first run
// only pages modified since a stored "lastmodified" watermark are re-fetched.
//
// Auth follows the Atlassian conventions: User (email) + APIToken does basic
// auth (Confluence Cloud API tokens), APIToken alone is sent as a Bearer
// token (Data Center personal access tokens).
package confluence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/websource"
)

const (
	// pageLimit is the REST page size for content listing.
	pageLimit = 50
)

// Fetcher syncs one Confluence space per collection.
type Fetcher struct {
	client *http.Client
}

// Config is owned entirely by the Confluence provider. Auth follows the
// Atlassian conventions: User+APIToken does basic auth (Cloud API tokens),
// APIToken alone is sent as a Bearer token (Data Center PATs).
//
// Every field is a types.Null so a partial update can be merged onto the stored
// config precisely (see MergeConfig): a field absent from the update JSON keeps
// the stored value, an explicit null clears it, and a value overrides it. Use
// resolve() to get plain values for the sync logic.
type Config struct {
	BaseURL  types.Null[string] `json:"base_url"`
	Space    types.Null[string] `json:"space,omitempty"`
	User     types.Null[string] `json:"user,omitempty"`
	APIToken types.Null[string] `json:"api_token,omitempty"`

	// RootPage, when set, restricts the sync to that page and every page below
	// it in the tree (its descendants), instead of the whole space. This lets
	// several sub-trees of one space be tracked as separate keyed sources
	// (e.g. one collection for "Delivery Support Documentation" and its
	// children). It is a Confluence page id (numeric, as in the page URL).
	RootPage types.Null[string] `json:"root_page,omitempty"`
	// IncludeRoot controls whether the RootPage page itself is indexed in
	// addition to its descendants (default: true).
	IncludeRoot types.Null[bool] `json:"include_root,omitempty"`

	IncludeLabels types.Null[[]string] `json:"include_labels,omitempty"`
	ExcludeLabels types.Null[[]string] `json:"exclude_labels,omitempty"`

	// FullResyncSchedule is a hardloop cron expression controlling when a full,
	// non-incremental pass reconciles remotely-deleted pages.
	FullResyncSchedule types.Null[string] `json:"full_resync_schedule,omitempty"`
	// FullResyncEvery is retained only to read configs persisted before full
	// resyncs became cron-based.
	FullResyncEvery types.Null[string] `json:"full_resync_every,omitempty"`

	// MaxPages caps how many results a single sync walks; 0 (the default) is
	// uncapped. Pages are streamed as they are converted, so the walk costs
	// the same memory at any size and the cap exists only to bound the time
	// and API spend of one run against a misconfigured query.
	//
	// A capped run cannot reconcile deletions: it reports an incomplete sweep,
	// which stops the manager from reading "not seen" as "deleted remotely".
	// Set it only if you want that trade.
	MaxPages types.Null[int] `json:"max_pages,omitempty"`
}

// resolvedConfig is the plain, validated view of a Config used by the sync
// logic. Strings are trimmed, the base URL has its trailing slash removed and
// IncludeRoot defaults to true.
type resolvedConfig struct {
	BaseURL            string
	Space              string
	User               string
	APIToken           string
	RootPage           string
	IncludeRoot        bool
	IncludeLabels      []string
	ExcludeLabels      []string
	FullResyncSchedule string
	MaxPages           int
}

// resolve flattens the nullable config into plain values with defaults applied.
func (c Config) resolve() resolvedConfig {
	includeRoot := true // default when unset
	if c.IncludeRoot.Valid {
		includeRoot = c.IncludeRoot.V
	}

	return resolvedConfig{
		BaseURL:       strings.TrimRight(strings.TrimSpace(c.BaseURL.ValueOrZero()), "/"),
		Space:         strings.TrimSpace(c.Space.ValueOrZero()),
		User:          strings.TrimSpace(c.User.ValueOrZero()),
		APIToken:      c.APIToken.ValueOrZero(),
		RootPage:      strings.TrimSpace(c.RootPage.ValueOrZero()),
		IncludeRoot:   includeRoot,
		IncludeLabels: c.IncludeLabels.ValueOrZero(),
		ExcludeLabels: c.ExcludeLabels.ValueOrZero(),
		FullResyncSchedule: websource.FullResyncSchedule(
			c.FullResyncSchedule.ValueOrZero(), c.FullResyncEvery.ValueOrZero()),
		MaxPages: c.MaxPages.ValueOrZero(),
	}
}

type configView struct {
	BaseURL            string   `json:"base_url"`
	Space              string   `json:"space,omitempty"`
	User               string   `json:"user,omitempty"`
	APITokenSet        bool     `json:"api_token_set"`
	RootPage           string   `json:"root_page,omitempty"`
	IncludeRoot        *bool    `json:"include_root,omitempty"`
	IncludeLabels      []string `json:"include_labels,omitempty"`
	ExcludeLabels      []string `json:"exclude_labels,omitempty"`
	FullResyncSchedule string   `json:"full_resync_schedule,omitempty"`
	MaxPages           int      `json:"max_pages,omitempty"`
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: &http.Client{Timeout: 60 * time.Second}}
}

// decodeConfig unmarshals the raw config and returns its resolved (plain) form.
func decodeConfig(raw json.RawMessage) (resolvedConfig, error) {
	cfg, err := decodeRawConfig(raw)
	if err != nil {
		return resolvedConfig{}, err
	}

	return cfg.resolve(), nil
}

// decodeRawConfig unmarshals the raw config keeping the nullable fields, so
// MergeConfig can tell set fields from absent ones.
func decodeRawConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode confluence config; %w", err)
		}
	}

	return cfg, nil
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return err
	}
	if cfg.BaseURL == "" || (!strings.HasPrefix(cfg.BaseURL, "https://") && !strings.HasPrefix(cfg.BaseURL, "http://")) {
		return fmt.Errorf("confluence base_url must be an http(s) URL")
	}
	if cfg.Space == "" && cfg.RootPage == "" {
		return fmt.Errorf("confluence requires a space key or a root_page id")
	}
	// The root page id is interpolated into a CQL clause and a request path,
	// so it has to look like the numeric id the UI asks for.
	if cfg.RootPage != "" && !isDigits(cfg.RootPage) {
		return fmt.Errorf("confluence root_page must be a numeric page id, got %q", cfg.RootPage)
	}
	if err := websource.ValidateFullResyncSchedule(cfg.FullResyncSchedule); err != nil {
		return err
	}

	return nil
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// MergeConfig merges an update onto the stored config so partial updates (e.g.
// changing only the description, which sends no config) do not wipe connection
// settings. Each field uses types.Null semantics: a field absent from the
// update keeps the stored value, an explicit null clears it, and a value
// overrides it. A blank api_token is treated as "keep the stored secret" since
// tokens are write-only and never round-trip to the client.
func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	next, err := decodeRawConfig(update)
	if err != nil {
		return nil, err
	}

	if len(current) != 0 {
		prev, err := decodeRawConfig(current)
		if err != nil {
			return nil, err
		}

		next.BaseURL = websource.MergeNull(next.BaseURL, prev.BaseURL)
		next.Space = websource.MergeNull(next.Space, prev.Space)
		next.User = websource.MergeNull(next.User, prev.User)
		next.RootPage = websource.MergeNull(next.RootPage, prev.RootPage)
		next.IncludeRoot = websource.MergeNull(next.IncludeRoot, prev.IncludeRoot)
		next.IncludeLabels = websource.MergeNull(next.IncludeLabels, prev.IncludeLabels)
		next.ExcludeLabels = websource.MergeNull(next.ExcludeLabels, prev.ExcludeLabels)
		next.FullResyncSchedule = websource.MergeNull(next.FullResyncSchedule, prev.FullResyncSchedule)
		next.FullResyncEvery = websource.MergeNull(next.FullResyncEvery, prev.FullResyncEvery)
		next.MaxPages = websource.MergeNull(next.MaxPages, prev.MaxPages)

		// Tokens are write-only: an absent or blank incoming token keeps the
		// stored one; only a non-empty value replaces it.
		if next.APIToken.ValueOrZero() == "" {
			next.APIToken = prev.APIToken
		}
	}

	raw, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode confluence config; %w", err)
	}
	if err := f.Validate(raw); err != nil {
		return nil, err
	}

	return raw, nil
}

func (f *Fetcher) ConfigView(raw json.RawMessage) any {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return nil
	}
	includeRoot := cfg.IncludeRoot

	return configView{
		BaseURL: cfg.BaseURL, Space: cfg.Space, User: cfg.User,
		APITokenSet: cfg.APIToken != "", RootPage: cfg.RootPage,
		IncludeRoot:   &includeRoot,
		IncludeLabels: cfg.IncludeLabels, ExcludeLabels: cfg.ExcludeLabels,
		FullResyncSchedule: cfg.FullResyncSchedule,
		MaxPages:           cfg.MaxPages,
	}
}

func (f *Fetcher) FullResyncSpec(raw json.RawMessage) (string, error) {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return "", err
	}
	if err := websource.ValidateFullResyncSchedule(cfg.FullResyncSchedule); err != nil {
		return "", err
	}

	return cfg.FullResyncSchedule, nil
}

// Preview validates an unsaved Confluence scope and counts pages that would be
// indexed after label filtering. It requests metadata only, avoiding page-body
// downloads and HTML conversion. A configured root page is fetched even when
// excluded from indexing so an invalid or inaccessible root fails the test.
func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var out websource.PreviewResult
	if err := f.Validate(raw); err != nil {
		return out, err
	}

	cfg, err := decodeConfig(raw)
	if err != nil {
		return out, err
	}
	out.Limit = cfg.MaxPages

	next := firstEndpointWithExpand(cfg, "", "metadata.labels")
	seen := map[string]bool{}
	for next != "" {
		if seen[next] {
			return out, fmt.Errorf("confluence pagination repeated cursor %q", next)
		}
		seen[next] = true

		list, err := f.listContent(ctx, cfg, absoluteEndpoint(cfg.BaseURL, next))
		if err != nil {
			return out, err
		}

		processed := 0
		for _, page := range list.Results {
			if cfg.MaxPages > 0 && out.Scanned >= cfg.MaxPages {
				out.Truncated = true

				break
			}
			out.Scanned++
			processed++
			if labelSelected(page, cfg.IncludeLabels, cfg.ExcludeLabels) {
				out.ItemCount++
			}
		}
		if out.Truncated || (cfg.MaxPages > 0 && out.Scanned >= cfg.MaxPages && (processed < len(list.Results) || list.Links.Next != "")) {
			out.Truncated = true

			break
		}
		if len(list.Results) == 0 {
			break
		}

		next = list.Links.Next
	}

	if cfg.RootPage != "" {
		root, err := f.fetchOneExpanded(ctx, cfg, cfg.BaseURL, cfg.RootPage, "metadata.labels")
		if err != nil {
			return out, fmt.Errorf("validate confluence root page %s; %w", cfg.RootPage, err)
		}
		out.Scanned++
		if cfg.IncludeRoot && labelSelected(*root, cfg.IncludeLabels, cfg.ExcludeLabels) {
			out.ItemCount++
		}
	}

	return out, nil
}

// contentPage mirrors the fields we consume from the Confluence content API.
type contentPage struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Metadata struct {
		Labels struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		} `json:"labels"`
	} `json:"metadata"`
	Version struct {
		// When is the page's last-modified time (RFC3339), used to advance the
		// incremental-sync watermark.
		When string `json:"when"`
	} `json:"version"`
	// Ancestors are the page's parent chain from the space root down to its
	// immediate parent (Confluence returns them in that order). Their titles
	// form a breadcrumb prepended to the markdown so a page with a weak title
	// (e.g. "Core implementation", "QA") carries its position in the tree into
	// its embedding and to the model, without mixing in sibling content.
	Ancestors []struct {
		Title string `json:"title"`
	} `json:"ancestors"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

// breadcrumb renders the ancestor titles as "A › B › C" (excluding the page
// itself). Empty when the page has no ancestors.
func (p contentPage) breadcrumb() string {
	if len(p.Ancestors) == 0 {
		return ""
	}
	parts := make([]string, 0, len(p.Ancestors))
	for _, a := range p.Ancestors {
		if t := strings.TrimSpace(a.Title); t != "" {
			parts = append(parts, t)
		}
	}

	return strings.Join(parts, " › ")
}

type contentList struct {
	Results []contentPage `json:"results"`
	Size    int           `json:"size"`
	Links   struct {
		// Next is the relative path to the next result page, present while more
		// results remain. Both the content listing and the CQL search return
		// it, so paging is uniform across space and subtree mode.
		Next string `json:"next"`
	} `json:"_links"`
}

// confluenceTimeLayout is the datetime format CQL accepts in a "lastmodified"
// clause (minute granularity).
const confluenceTimeLayout = "2006-01-02 15:04"

// syncState is the opaque provider watermark persisted between syncs. Watermark
// is the highest page last-modified time ingested so far, in CQL format; the
// next incremental fetch asks only for pages modified at or after it. FullAt is
// the time of the last full (non-incremental) pass, used to schedule periodic
// full sweeps that reconcile remotely-deleted pages.
type syncState struct {
	Watermark string    `json:"watermark,omitempty"`
	FullAt    time.Time `json:"full_at,omitzero"`
	// V is the slug generation of the records this state describes. A stored
	// state older than slugGeneration was written when slugs still embedded the
	// page title, so the next sweep must be a full one (see Fetch).
	V int `json:"v,omitempty"`
}

// slugGeneration is bumped whenever the slug format changes, which invalidates
// every stored record of the collection.
//
// Generation 2 dropped the title from the slug. A slug is the identity of a
// page across syncs and the name of its markdown file, so deriving it from a
// mutable field meant that renaming a Confluence page produced a second copy of
// it: the incremental run saw the bumped last-modified date, emitted the page
// under a new slug, and the old record, file and vectors stayed behind — a
// pruning pass would have removed them, but pruning needs a complete sweep, and
// incremental runs are never that. Search then returned the same document
// twice, one copy with a stale title, accumulating with every rename.
const slugGeneration = 2

// Fetch lists the current pages of the configured space (or, when root_page is
// set, that page and all its descendants), applies the label filters and
// converts each page to markdown. It is incremental: after the first full sync
// it fetches only pages modified since the stored watermark (via CQL
// lastmodified) and returns an advanced watermark, so a large tree is not
// re-listed, re-fetched or re-embedded every cycle. The persisted page records
// are ignored: Confluence is a discovery source, the remote tree is the truth.
//
// Pages are emitted as they are converted, so a space of any size is walked in
// constant memory and a full sweep can run to completion. Completion is what is
// reported back: only a full (non-incremental) walk that reached the end of the
// result set claims Complete, because only then does "not seen" mean "deleted".
func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, rawState json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decodeConfig(col.Config)
	if err != nil {
		return nil, err
	}

	base := cfg.BaseURL
	if base == "" {
		return nil, fmt.Errorf("confluence base_url is required")
	}
	if cfg.Space == "" && cfg.RootPage == "" {
		return nil, fmt.Errorf("confluence requires a space key or a root_page id")
	}

	var state syncState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &state)
	}

	// Periodically force a full pass so pages deleted remotely (which an
	// incremental "lastmodified >=" query never returns) are reconciled. A slug
	// format change forces one too: the stored records are keyed by the old
	// format, so only a complete sweep can re-emit every page under the new one
	// and prune the old-format leftovers in the same run.
	full := state.V < slugGeneration || websource.FullResyncDue(state.FullAt, cfg.FullResyncSchedule, time.Now())
	watermark := state.Watermark
	if full {
		watermark = ""
	}
	incremental := watermark != ""

	var maxSeen time.Time

	// A zero cap is uncapped: the walk streams, so its memory does not grow
	// with the space and there is nothing to protect against by stopping early.
	capped := cfg.MaxPages > 0
	truncated := false

	next := firstEndpoint(cfg, watermark)
	for count := 0; next != ""; {
		if capped && count >= cfg.MaxPages {
			truncated = true

			break
		}

		list, err := f.listContent(ctx, cfg, absoluteEndpoint(base, next))
		if err != nil {
			return nil, err
		}

		processed := 0
		for _, page := range list.Results {
			if capped && count >= cfg.MaxPages {
				truncated = true

				break
			}
			count++
			processed++

			if w := parseConfluenceTime(page.Version.When); w.After(maxSeen) {
				maxSeen = w
			}

			if !labelSelected(page, cfg.IncludeLabels, cfg.ExcludeLabels) {
				continue
			}

			if err := emit(pageToRemote(base, page)); err != nil {
				return nil, err
			}
		}
		if truncated || (capped && count >= cfg.MaxPages && (processed < len(list.Results) || list.Links.Next != "")) {
			truncated = true

			break
		}

		// Confluence pages a cursor without publishing a result-set size, so
		// only the running count is known; the caller shows it as an
		// indeterminate phase with a live counter.
		progress.Report(ctx, count, 0)

		// The cursor is the only thing that ends this walk, and it comes from
		// the server. An instance that keeps returning a next link past the
		// last result — or one that returns the same cursor twice — would spin
		// here forever while holding the collection's sync lock and a queue
		// slot. An empty page means there is nothing left regardless of what
		// the cursor claims, and a repeated cursor is not progress.
		if len(list.Results) == 0 || list.Links.Next == next {
			break
		}

		next = list.Links.Next
	}

	// In subtree mode also index the root page itself (the CQL descendant query
	// returns only pages below it). Skip it on incremental runs when it was not
	// modified since the watermark.
	if cfg.RootPage != "" && cfg.IncludeRoot {
		if root, err := f.fetchOne(ctx, cfg, base, cfg.RootPage); err != nil {
			// Reporting this as a page would invent a slug ("<id>-root") that
			// the success path never produces, so the record could never be
			// updated by a later run — only pruned by a full sweep. The root's
			// previously synced copy stays as it is; the failure is logged.
			slog.Warn("confluence root page fetch failed; keeping its previous copy",
				"source", col.Name, "root_page", cfg.RootPage, "error", err)
		} else {
			w := parseConfluenceTime(root.Version.When)
			if w.After(maxSeen) {
				maxSeen = w
			}
			rootChanged := !incremental || !w.Before(parseConfluenceTime(watermark))
			if rootChanged && labelSelected(*root, cfg.IncludeLabels, cfg.ExcludeLabels) {
				if err := emit(pageToRemote(base, *root)); err != nil {
					return nil, err
				}
			}
		}
	}

	// Advance the watermark to the newest page seen, stepping back a minute so
	// pages modified within the boundary minute are not skipped next run (the
	// manager's hash check makes the small re-fetch a no-op).
	nextWatermark := state.Watermark
	if !maxSeen.IsZero() {
		nextWatermark = maxSeen.Add(-time.Minute).Format(confluenceTimeLayout)
	}

	// A truncated full pass still counts as "attempted" so the watermark is
	// allowed to advance on the next run. Retrying the full pass instead would
	// clear the watermark every time and re-walk the same prefix forever,
	// leaving everything past the cap permanently unsynced. The cost of that
	// choice — deletions are never reconciled on a capped collection — is
	// carried by Complete below, not hidden.
	fullAt := state.FullAt
	if full {
		fullAt = time.Now()
	}

	nextState, err := json.Marshal(syncState{Watermark: nextWatermark, FullAt: fullAt, V: slugGeneration})
	if err != nil {
		return nil, fmt.Errorf("encode confluence sync state; %w", err)
	}

	if truncated {
		slog.Warn("confluence sync hit max_pages; deletions will not be reconciled this run",
			"source", col.Name, "max_pages", cfg.MaxPages)
	}

	return &websource.FetchResult{
		Complete: !incremental && !truncated,
		State:    nextState,
	}, nil
}

// pageToRemote converts one Confluence page to a RemotePage, recording a
// per-page conversion error rather than failing the whole sync.
func pageToRemote(base string, page contentPage) websource.RemotePage {
	remote := websource.RemotePage{
		// The page id alone: it is the only part of a Confluence page that
		// does not change under the user's hands. The title used to be
		// appended for a readable filename, but a slug is an identity, and
		// tying an identity to a mutable field duplicated the page on every
		// rename (see slugGeneration). The title still reaches search through
		// the page record and the markdown heading.
		Slug:      page.ID,
		Title:     page.Title,
		URL:       base + page.Links.WebUI,
		UpdatedAt: parseConfluenceTime(page.Version.When),
	}
	if md, err := websource.MarkdownFromHTML(page.Body.Storage.Value, remote.URL); err != nil {
		remote.Err = fmt.Errorf("convert page %s (%s); %w", page.ID, page.Title, err)
	} else {
		// Prepend the ancestor breadcrumb so a weakly-titled page carries its
		// place in the space tree into both its embedding and the model's view,
		// without mixing in sibling content.
		if bc := page.breadcrumb(); bc != "" {
			md = "> " + bc + "\n\n" + md
		}
		remote.Markdown = md
	}

	return remote
}

// firstEndpoint returns the relative URL of the first result page. Both space
// and subtree mode use CQL so the incremental "lastmodified" clause and the
// ascending order (for a monotonic watermark) apply uniformly. Both return a
// "_links.next" cursor for subsequent pages.
func firstEndpoint(cfg resolvedConfig, watermark string) string {
	return firstEndpointWithExpand(cfg, watermark, "body.storage,metadata.labels,version,ancestors")
}

func firstEndpointWithExpand(cfg resolvedConfig, watermark, expand string) string {
	clauses := []string{"type = page"}
	if cfg.Space != "" {
		clauses = append(clauses, fmt.Sprintf("space = %q", cfg.Space))
	}
	if cfg.RootPage != "" {
		clauses = append(clauses, "ancestor = "+cfg.RootPage)
	}
	if watermark != "" {
		clauses = append(clauses, fmt.Sprintf("lastmodified >= %q", watermark))
	}

	cql := strings.Join(clauses, " AND ") + " ORDER BY lastmodified ASC"

	params := url.Values{}
	params.Set("cql", cql)
	params.Set("limit", strconv.Itoa(pageLimit))
	params.Set("expand", expand)

	return "/rest/api/content/search?" + params.Encode()
}

// parseConfluenceTime parses a page's version.when timestamp (RFC3339). A zero
// time is returned when it cannot be parsed.
func parseConfluenceTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	// CQL watermark format fallback (our own stored value).
	if t, err := time.Parse(confluenceTimeLayout, s); err == nil {
		return t
	}

	return time.Time{}
}

// fetchOne retrieves a single page by id with body + labels (used for the root
// page in subtree mode).
func (f *Fetcher) fetchOne(ctx context.Context, cfg resolvedConfig, base, id string) (*contentPage, error) {
	return f.fetchOneExpanded(ctx, cfg, base, id, "body.storage,metadata.labels,version,ancestors")
}

func (f *Fetcher) fetchOneExpanded(ctx context.Context, cfg resolvedConfig, base, id, expand string) (*contentPage, error) {
	params := url.Values{}
	params.Set("expand", expand)
	endpoint := base + "/rest/api/content/" + url.PathEscape(id) + "?" + params.Encode()

	body, err := f.get(ctx, cfg, endpoint)
	if err != nil {
		return nil, err
	}

	var page contentPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("decode confluence page; %w", err)
	}

	return &page, nil
}

func absoluteEndpoint(base, endpoint string) string {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	baseURL, err := url.Parse(base)
	if err == nil {
		basePath := strings.TrimRight(baseURL.Path, "/")
		if basePath != "" && strings.HasPrefix(endpoint, basePath+"/") {
			return baseURL.Scheme + "://" + baseURL.Host + endpoint
		}
	}

	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(endpoint, "/")
}

// listContent fetches one result page from a fully-formed endpoint URL.
func (f *Fetcher) listContent(ctx context.Context, cfg resolvedConfig, endpoint string) (*contentList, error) {
	body, err := f.get(ctx, cfg, endpoint)
	if err != nil {
		return nil, err
	}

	var list contentList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode confluence response; %w", err)
	}

	return &list, nil
}

// get performs an authenticated GET and returns the response body.
func (f *Fetcher) get(ctx context.Context, cfg resolvedConfig, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request; %w", err)
	}

	req.Header.Set("Accept", "application/json")

	switch {
	case cfg.User != "":
		req.SetBasicAuth(cfg.User, cfg.APIToken)
	case cfg.APIToken != "":
		req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	}

	res, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request confluence; %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read confluence response; %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("confluence request: status %s: %s", res.Status, truncate(string(body), 300))
	}

	return body, nil
}

// FetchImage downloads a standard Confluence image URL. Provider credentials
// are sent only to the configured Confluence origin and only after the global
// authenticated-image opt-in is enabled.
func (f *Fetcher) FetchImage(ctx context.Context, col *websource.Collection, pageURL, imageURL string, maxBytes int64, allowAuthenticated bool) (websource.ImageContent, error) {
	cfg, err := decodeConfig(col.Config)
	if err != nil {
		return websource.ImageContent{}, err
	}
	privateOrigin := pageURL
	if privateOrigin == "" {
		privateOrigin = cfg.BaseURL
	}
	allowPrivate := allowAuthenticated && websource.SameOrigin(privateOrigin, imageURL)
	if err := websource.ValidateImageURL(imageURL, allowPrivate); err != nil {
		return websource.ImageContent{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return websource.ImageContent{}, fmt.Errorf("build confluence image request; %w", err)
	}
	req.Header.Set("Accept", "image/png,image/jpeg,image/gif")

	authenticated := false
	if allowAuthenticated && websource.SameOrigin(cfg.BaseURL, imageURL) {
		switch {
		case cfg.User != "":
			req.SetBasicAuth(cfg.User, cfg.APIToken)
			authenticated = true
		case cfg.APIToken != "":
			req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
			authenticated = true
		}
	}

	privateHost := ""
	if allowPrivate {
		if parsed, parseErr := url.Parse(imageURL); parseErr == nil {
			privateHost = parsed.Hostname()
		}
	}
	client := websource.ImageHTTPClient(f.client, privateHost)
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if authenticated && !websource.SameOrigin(imageURL, req.URL.String()) {
			return fmt.Errorf("%w: authenticated image redirect changed origin", websource.ErrImageUnsupported)
		}
		redirectAllowsPrivate := allowPrivate && websource.SameOrigin(imageURL, req.URL.String())
		return websource.ValidateImageURL(req.URL.String(), redirectAllowsPrivate)
	}
	res, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return websource.ImageContent{}, fmt.Errorf("fetch confluence image; %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return websource.ImageContent{}, fmt.Errorf("%w: confluence image status %s", websource.ErrImageUnsupported, res.Status)
	}
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return websource.ImageContent{}, fmt.Errorf("read confluence image; %w", err)
	}
	if int64(len(body)) > maxBytes {
		return websource.ImageContent{}, fmt.Errorf("%w: confluence image exceeds %d bytes", websource.ErrImageUnsupported, maxBytes)
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(res.Header.Get("Content-Type"), ";")[0]))
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = http.DetectContentType(body)
	}
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif":
	default:
		return websource.ImageContent{}, fmt.Errorf("%w: confluence image content type %q", websource.ErrImageUnsupported, mediaType)
	}
	return websource.ImageContent{Data: body, MediaType: mediaType, Authenticated: authenticated}, nil
}

// labelSelected applies the include/exclude label filters to one page.
func labelSelected(page contentPage, include, exclude []string) bool {
	labels := map[string]bool{}
	for _, l := range page.Metadata.Labels.Results {
		labels[strings.ToLower(l.Name)] = true
	}

	for _, l := range exclude {
		if labels[strings.ToLower(strings.TrimSpace(l))] {
			return false
		}
	}

	if len(include) == 0 {
		return true
	}

	for _, l := range include {
		if labels[strings.ToLower(strings.TrimSpace(l))] {
			return true
		}
	}

	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "…"
}
