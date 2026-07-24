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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/service/websource"
)

const (
	// pageLimit is the REST page size for content listing.
	pageLimit = 50
	// maxPages caps a space sync so a runaway space cannot grind the system.
	maxPages = 5000
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

	// FullResyncEvery is a Go duration ("24h") controlling how often a full,
	// non-incremental pass runs to reconcile remotely-deleted pages. Empty or
	// invalid uses the default (24h).
	FullResyncEvery types.Null[string] `json:"full_resync_every,omitempty"`
}

// resolvedConfig is the plain, validated view of a Config used by the sync
// logic. Strings are trimmed, the base URL has its trailing slash removed and
// IncludeRoot defaults to true.
type resolvedConfig struct {
	BaseURL         string
	Space           string
	User            string
	APIToken        string
	RootPage        string
	IncludeRoot     bool
	IncludeLabels   []string
	ExcludeLabels   []string
	FullResyncEvery string
}

// resolve flattens the nullable config into plain values with defaults applied.
func (c Config) resolve() resolvedConfig {
	includeRoot := true // default when unset
	if c.IncludeRoot.Valid {
		includeRoot = c.IncludeRoot.V
	}

	return resolvedConfig{
		BaseURL:         strings.TrimRight(strings.TrimSpace(c.BaseURL.ValueOrZero()), "/"),
		Space:           strings.TrimSpace(c.Space.ValueOrZero()),
		User:            strings.TrimSpace(c.User.ValueOrZero()),
		APIToken:        c.APIToken.ValueOrZero(),
		RootPage:        strings.TrimSpace(c.RootPage.ValueOrZero()),
		IncludeRoot:     includeRoot,
		IncludeLabels:   c.IncludeLabels.ValueOrZero(),
		ExcludeLabels:   c.ExcludeLabels.ValueOrZero(),
		FullResyncEvery: strings.TrimSpace(c.FullResyncEvery.ValueOrZero()),
	}
}

type configView struct {
	BaseURL         string   `json:"base_url"`
	Space           string   `json:"space,omitempty"`
	User            string   `json:"user,omitempty"`
	APITokenSet     bool     `json:"api_token_set"`
	RootPage        string   `json:"root_page,omitempty"`
	IncludeRoot     *bool    `json:"include_root,omitempty"`
	IncludeLabels   []string `json:"include_labels,omitempty"`
	ExcludeLabels   []string `json:"exclude_labels,omitempty"`
	FullResyncEvery string   `json:"full_resync_every,omitempty"`
}

// fullResyncEvery parses the configured interval, falling back to the default.
func (c resolvedConfig) fullResyncEvery() time.Duration {
	if c.FullResyncEvery == "" {
		return websource.DefaultFullResyncEvery
	}
	d, err := time.ParseDuration(c.FullResyncEvery)
	if err != nil || d <= 0 {
		return websource.DefaultFullResyncEvery
	}

	return d
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

// mergeNull returns the update value when the field was present in the update
// JSON (set to a value OR explicitly null), otherwise the stored value. It
// implements the "absent = keep, null = clear, value = override" merge rule
// backed by types.Null's ParsedNull marker.
func mergeNull[T any](update, stored types.Null[T]) types.Null[T] {
	if update.Valid || update.ParsedNull {
		return update
	}

	return stored
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
	return nil
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

		next.BaseURL = mergeNull(next.BaseURL, prev.BaseURL)
		next.Space = mergeNull(next.Space, prev.Space)
		next.User = mergeNull(next.User, prev.User)
		next.RootPage = mergeNull(next.RootPage, prev.RootPage)
		next.IncludeRoot = mergeNull(next.IncludeRoot, prev.IncludeRoot)
		next.IncludeLabels = mergeNull(next.IncludeLabels, prev.IncludeLabels)
		next.ExcludeLabels = mergeNull(next.ExcludeLabels, prev.ExcludeLabels)
		next.FullResyncEvery = mergeNull(next.FullResyncEvery, prev.FullResyncEvery)

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
		FullResyncEvery: cfg.FullResyncEvery,
	}
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
}

// Fetch lists the current pages of the configured space (or, when root_page is
// set, that page and all its descendants), applies the label filters and
// converts each page to markdown. It is incremental: after the first full sync
// it fetches only pages modified since the stored watermark (via CQL
// lastmodified) and returns an advanced watermark, so a large tree is not
// re-listed, re-fetched or re-embedded every cycle. The persisted page records
// are ignored: Confluence is a discovery source, the remote tree is the truth.
func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, rawState json.RawMessage) (*websource.FetchResult, error) {
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
	// incremental "lastmodified >=" query never returns) are reconciled.
	full := websource.FullResyncDue(state.FullAt, cfg.fullResyncEvery())
	watermark := state.Watermark
	if full {
		watermark = ""
	}
	incremental := watermark != ""

	var (
		out     []websource.RemotePage
		maxSeen time.Time
	)

	next := firstEndpoint(cfg, watermark)
	for count := 0; next != "" && count < maxPages; {
		list, err := f.listContent(ctx, cfg, base+next)
		if err != nil {
			return nil, err
		}

		for _, page := range list.Results {
			count++

			if w := parseConfluenceTime(page.Version.When); w.After(maxSeen) {
				maxSeen = w
			}

			if !labelSelected(page, cfg.IncludeLabels, cfg.ExcludeLabels) {
				continue
			}

			out = append(out, pageToRemote(base, page))
		}

		next = list.Links.Next
	}

	// In subtree mode also index the root page itself (the CQL descendant query
	// returns only pages below it). Skip it on incremental runs when it was not
	// modified since the watermark.
	if cfg.RootPage != "" && cfg.IncludeRoot {
		if root, err := f.fetchOne(ctx, cfg, base, cfg.RootPage); err != nil {
			out = append(out, websource.RemotePage{
				Slug: cfg.RootPage + "-root",
				Err:  fmt.Errorf("fetch root page %s; %w", cfg.RootPage, err),
			})
		} else {
			w := parseConfluenceTime(root.Version.When)
			if w.After(maxSeen) {
				maxSeen = w
			}
			rootChanged := !incremental || !w.Before(parseConfluenceTime(watermark))
			if rootChanged && labelSelected(*root, cfg.IncludeLabels, cfg.ExcludeLabels) {
				out = append(out, pageToRemote(base, *root))
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

	fullAt := state.FullAt
	if full {
		fullAt = time.Now()
	}

	nextState, err := json.Marshal(syncState{Watermark: nextWatermark, FullAt: fullAt})
	if err != nil {
		return nil, fmt.Errorf("encode confluence sync state; %w", err)
	}

	return &websource.FetchResult{
		Pages:       out,
		Incremental: incremental,
		State:       nextState,
	}, nil
}

// pageToRemote converts one Confluence page to a RemotePage, recording a
// per-page conversion error rather than failing the whole sync.
func pageToRemote(base string, page contentPage) websource.RemotePage {
	remote := websource.RemotePage{
		Slug:      page.ID + "-" + websource.Slugify(page.Title),
		Title:     page.Title,
		URL:       base + page.Links.WebUI,
		UpdatedAt: parseConfluenceTime(page.Version.When),
	}
	if md, err := websource.MarkdownFromHTML(page.Body.Storage.Value); err != nil {
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
	params.Set("expand", "body.storage,metadata.labels,version,ancestors")

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
	params := url.Values{}
	params.Set("expand", "body.storage,metadata.labels,version,ancestors")
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
