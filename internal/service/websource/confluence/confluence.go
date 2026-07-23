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
type Config struct {
	BaseURL  string `json:"base_url"`
	Space    string `json:"space,omitempty"`
	User     string `json:"user,omitempty"`
	APIToken string `json:"api_token,omitempty"`

	// RootPage, when set, restricts the sync to that page and every page below
	// it in the tree (its descendants), instead of the whole space. This lets
	// several sub-trees of one space be tracked as separate keyed sources
	// (e.g. one collection for "Delivery Support Documentation" and its
	// children). It is a Confluence page id (numeric, as in the page URL).
	RootPage string `json:"root_page,omitempty"`
	// IncludeRoot controls whether the RootPage page itself is indexed in
	// addition to its descendants (default: true).
	IncludeRoot *bool `json:"include_root,omitempty"`

	IncludeLabels []string `json:"include_labels,omitempty"`
	ExcludeLabels []string `json:"exclude_labels,omitempty"`

	// FullResyncEvery is a Go duration ("24h") controlling how often a full,
	// non-incremental pass runs to reconcile remotely-deleted pages. Empty or
	// invalid uses the default (24h).
	FullResyncEvery string `json:"full_resync_every,omitempty"`
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
func (c Config) fullResyncEvery() time.Duration {
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

func decodeConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode confluence config; %w", err)
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Space = strings.TrimSpace(cfg.Space)
	cfg.User = strings.TrimSpace(cfg.User)
	cfg.RootPage = strings.TrimSpace(cfg.RootPage)
	return cfg, nil
}

// includeRoot reports whether the root page itself is indexed (default true).
func (c Config) includeRoot() bool {
	return c.IncludeRoot == nil || *c.IncludeRoot
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

func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	next, err := decodeConfig(update)
	if err != nil {
		return nil, err
	}
	if next.APIToken == "" && len(current) != 0 {
		prev, err := decodeConfig(current)
		if err != nil {
			return nil, err
		}
		next.APIToken = prev.APIToken
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
	return configView{
		BaseURL: cfg.BaseURL, Space: cfg.Space, User: cfg.User,
		APITokenSet: cfg.APIToken != "", RootPage: cfg.RootPage,
		IncludeRoot:   cfg.IncludeRoot,
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
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
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
	if cfg.RootPage != "" && cfg.includeRoot() {
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
		Slug:  page.ID + "-" + websource.Slugify(page.Title),
		Title: page.Title,
		URL:   base + page.Links.WebUI,
	}
	if md, err := websource.MarkdownFromHTML(page.Body.Storage.Value); err != nil {
		remote.Err = fmt.Errorf("convert page %s (%s); %w", page.ID, page.Title, err)
	} else {
		remote.Markdown = md
	}

	return remote
}

// firstEndpoint returns the relative URL of the first result page. Both space
// and subtree mode use CQL so the incremental "lastmodified" clause and the
// ascending order (for a monotonic watermark) apply uniformly. Both return a
// "_links.next" cursor for subsequent pages.
func firstEndpoint(cfg Config, watermark string) string {
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
	params.Set("expand", "body.storage,metadata.labels,version")

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
func (f *Fetcher) fetchOne(ctx context.Context, cfg Config, base, id string) (*contentPage, error) {
	params := url.Values{}
	params.Set("expand", "body.storage,metadata.labels,version")
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
func (f *Fetcher) listContent(ctx context.Context, cfg Config, endpoint string) (*contentList, error) {
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
func (f *Fetcher) get(ctx context.Context, cfg Config, endpoint string) ([]byte, error) {
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
