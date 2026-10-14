// Package httpapi implements the generic "http" web-source type: it pages
// through any JSON REST endpoint, maps each record to a document with dotted
// field paths (or a Go template) and syncs incrementally with an optional
// "updated since" cursor. One configuration covers trackers, help desks and
// internal APIs (Zendesk, ServiceNow, Linear REST, YouTrack, Redmine, …)
// without provider-specific code.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/convert"
)

// Type is the collection type served by this fetcher.
const Type = "http"

// Pagination modes.
const (
	PageNone   = "none"
	PageOffset = "offset" // offset/limit query parameters
	PagePage   = "page"   // page number query parameter (1-based)
	PageCursor = "cursor" // cursor taken from a response field
	PageLink   = "link"   // RFC 8288 Link: rel="next" header
	PageNext   = "next"   // absolute next URL taken from a response field
)

const (
	defaultPageSize = 100
	maxPages        = 10_000
	maxResponse     = 64 << 20
)

// Config is the provider config.
type Config struct {
	URL      string            `json:"url"`
	Method   string            `json:"method,omitempty"`
	Body     string            `json:"body,omitempty"`
	User     string            `json:"user,omitempty"`
	Token    string            `json:"token,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	ItemsKey string            `json:"items_path,omitempty"`

	Pagination  string `json:"pagination,omitempty"`
	PageSize    int    `json:"page_size,omitempty"`
	OffsetParam string `json:"offset_param,omitempty"`
	LimitParam  string `json:"limit_param,omitempty"`
	PageParam   string `json:"page_param,omitempty"`
	CursorParam string `json:"cursor_param,omitempty"`
	CursorPath  string `json:"cursor_path,omitempty"`
	NextPath    string `json:"next_path,omitempty"`

	IDPath      string   `json:"id_path"`
	TitlePath   string   `json:"title_path,omitempty"`
	BodyPath    string   `json:"body_path,omitempty"`
	BodyFormat  string   `json:"body_format,omitempty"`
	URLPath     string   `json:"url_path,omitempty"`
	URLTemplate string   `json:"url_template,omitempty"`
	UpdatedPath string   `json:"updated_path,omitempty"`
	TeamPaths   []string `json:"team_paths,omitempty"`
	Template    string   `json:"template,omitempty"`

	SinceParam  string `json:"since_param,omitempty"`
	SinceFormat string `json:"since_format,omitempty"`

	MaxItems           int    `json:"max_items,omitempty"`
	FullResyncSchedule string `json:"full_resync_schedule,omitempty"`
}

func (c Config) normalize() Config {
	c.URL = strings.TrimSpace(c.URL)
	c.Method = strings.ToUpper(strings.TrimSpace(c.Method))
	if c.Method == "" {
		c.Method = http.MethodGet
	}
	if c.Pagination == "" {
		c.Pagination = PageNone
	}
	if c.PageSize <= 0 {
		c.PageSize = defaultPageSize
	}
	if c.OffsetParam == "" {
		c.OffsetParam = "offset"
	}
	if c.LimitParam == "" {
		c.LimitParam = "limit"
	}
	if c.PageParam == "" {
		c.PageParam = "page"
	}
	if c.CursorParam == "" {
		c.CursorParam = "cursor"
	}
	if c.SinceFormat == "" {
		c.SinceFormat = time.RFC3339
	}
	c.FullResyncSchedule = websource.FullResyncSchedule(c.FullResyncSchedule, "")

	return c
}

// Fetcher is the generic HTTP/JSON fetcher.
type Fetcher struct {
	websource.Schema
	client *http.Client
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: websource.DefaultHTTPClient(), Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "Generic HTTP / JSON API",
		Description: "Page through any JSON REST endpoint and map each record to a document with dotted field paths " +
			"(e.g. data.items, fields.summary) or a Go template. Covers Zendesk, ServiceNow, YouTrack, Redmine and internal APIs.",
		Fields: []websource.Field{
			{Key: "url", Label: "Endpoint URL", Kind: websource.FieldString, Required: true, Placeholder: "https://api.example.com/v1/tickets"},
			{Key: "method", Label: "Method", Kind: websource.FieldSelect, Options: []string{"GET", "POST"}, Default: "GET"},
			{Key: "body", Label: "Request body (POST, JSON)", Kind: websource.FieldText},
			{Key: "user", Label: "User (basic auth; empty = bearer token)", Kind: websource.FieldString},
			{Key: "token", Label: "API token", Kind: websource.FieldSecret},
			{Key: "headers", Label: "Extra headers", Kind: websource.FieldMap, Help: "e.g. X-Api-Key; values are write-only"},
			{Key: "items_path", Label: "Items path", Kind: websource.FieldString, Placeholder: "data.items", Help: "Dotted path of the record array; empty when the response is the array"},
			{Key: "pagination", Label: "Pagination", Kind: websource.FieldSelect, Options: []string{PageNone, PageOffset, PagePage, PageCursor, PageLink, PageNext}, Default: PageNone},
			{Key: "page_size", Label: "Page size", Kind: websource.FieldNumber, Default: defaultPageSize},
			{Key: "offset_param", Label: "Offset param", Kind: websource.FieldString, Placeholder: "offset"},
			{Key: "limit_param", Label: "Limit param", Kind: websource.FieldString, Placeholder: "limit"},
			{Key: "page_param", Label: "Page param", Kind: websource.FieldString, Placeholder: "page"},
			{Key: "cursor_param", Label: "Cursor param", Kind: websource.FieldString, Placeholder: "cursor"},
			{Key: "cursor_path", Label: "Next cursor path", Kind: websource.FieldString, Placeholder: "meta.next_cursor"},
			{Key: "next_path", Label: "Next URL path", Kind: websource.FieldString, Placeholder: "links.next"},
			{Key: "id_path", Label: "ID path", Kind: websource.FieldString, Required: true, Placeholder: "id"},
			{Key: "title_path", Label: "Title path", Kind: websource.FieldString, Placeholder: "subject"},
			{Key: "body_path", Label: "Body path", Kind: websource.FieldString, Placeholder: "description"},
			{Key: "body_format", Label: "Body format", Kind: websource.FieldSelect, Options: []string{"markdown", "html", "text"}, Default: "markdown"},
			{Key: "url_path", Label: "Link path", Kind: websource.FieldString, Placeholder: "html_url"},
			{Key: "url_template", Label: "Link template", Kind: websource.FieldString, Placeholder: "https://tracker.example.com/t/{{.id}}"},
			{Key: "updated_path", Label: "Updated-at path", Kind: websource.FieldString, Placeholder: "updated_at"},
			{Key: "team_paths", Label: "Team / owner paths", Kind: websource.FieldList, Placeholder: "group.name, assignee.team"},
			{Key: "template", Label: "Markdown template (optional)", Kind: websource.FieldText, Help: "Go text/template over the record, e.g. {{.subject}}\\n\\n{{.description}}; helpers: get, str, join, date, json"},
			{Key: "since_param", Label: "Updated-since param (incremental)", Kind: websource.FieldString, Placeholder: "updated_since"},
			{Key: "since_format", Label: "Since format", Kind: websource.FieldSelect, Options: []string{time.RFC3339, "unix", "unix_ms", "2006-01-02"}, Default: time.RFC3339},
			{Key: "max_items", Label: "Max items per sync (0 = no limit)", Kind: websource.FieldNumber},
			{Key: "full_resync_schedule", Label: "Full re-sync schedule (cron)", Kind: websource.FieldString, Placeholder: websource.DefaultFullResyncSchedule},
		},
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)

	return cfg.normalize(), err
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if err := websource.ValidHTTPURL(cfg.URL); err != nil {
		return fmt.Errorf("http url %w", err)
	}
	if strings.TrimSpace(cfg.IDPath) == "" {
		return fmt.Errorf("http id_path is required")
	}
	switch cfg.Pagination {
	case PageNone, PageOffset, PagePage, PageLink:
	case PageCursor:
		if cfg.CursorPath == "" {
			return fmt.Errorf("cursor pagination needs cursor_path")
		}
	case PageNext:
		if cfg.NextPath == "" {
			return fmt.Errorf("next pagination needs next_path")
		}
	default:
		return fmt.Errorf("unknown pagination %q", cfg.Pagination)
	}
	if cfg.Method != http.MethodGet && cfg.Method != http.MethodPost {
		return fmt.Errorf("method must be GET or POST")
	}
	if _, err := websource.CompileTemplate(cfg.Template); err != nil {
		return err
	}
	if _, err := websource.CompileTemplate(cfg.URLTemplate); err != nil {
		return fmt.Errorf("url_template: %w", err)
	}

	return websource.ValidateFullResyncSchedule(cfg.FullResyncSchedule)
}

func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	merged, err := f.MergeObjects(current, update)
	if err != nil {
		return nil, err
	}

	return merged, f.Validate(merged)
}

func (f *Fetcher) ConfigView(raw json.RawMessage) any { return f.View(raw) }

func (f *Fetcher) FullResyncSpec(raw json.RawMessage) (string, error) {
	cfg, err := decode(raw)
	if err != nil || cfg.SinceParam == "" {
		return "", err
	}

	return cfg.FullResyncSchedule, nil
}

type syncState struct {
	Since  time.Time `json:"since,omitzero"`
	FullAt time.Time `json:"full_at,omitzero"`
	Filter string    `json:"filter,omitempty"`
}

func filterSignature(cfg Config) string {
	return websource.Hash(strings.Join([]string{cfg.URL, cfg.Method, cfg.Body, cfg.ItemsKey, cfg.IDPath}, "\x00"))
}

// walk pages through the endpoint and calls fn for every record.
func (f *Fetcher) walk(ctx context.Context, cfg Config, since time.Time, fn func(map[string]any) (bool, error)) (truncated bool, err error) {
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return false, err
	}
	if cfg.SinceParam != "" && !since.IsZero() {
		q := base.Query()
		q.Set(cfg.SinceParam, formatSince(since, cfg.SinceFormat))
		base.RawQuery = q.Encode()
	}

	next := base.String()
	cursor := ""
	count := 0

	for page := 0; page < maxPages && next != ""; page++ {
		u, _ := url.Parse(next)
		if cfg.Pagination != PageLink && cfg.Pagination != PageNext || page == 0 {
			q := u.Query()
			switch cfg.Pagination {
			case PageOffset:
				q.Set(cfg.OffsetParam, strconv.Itoa(page*cfg.PageSize))
				q.Set(cfg.LimitParam, strconv.Itoa(cfg.PageSize))
			case PagePage:
				q.Set(cfg.PageParam, strconv.Itoa(page+1))
				q.Set(cfg.LimitParam, strconv.Itoa(cfg.PageSize))
			case PageCursor:
				if cursor != "" {
					q.Set(cfg.CursorParam, cursor)
				}
				q.Set(cfg.LimitParam, strconv.Itoa(cfg.PageSize))
			}
			u.RawQuery = q.Encode()
		}

		res, body, err := f.request(ctx, cfg, u.String())
		if err != nil {
			return false, err
		}
		records, ok := websource.Lookup(body, cfg.ItemsKey).([]any)
		if !ok {
			if websource.Lookup(body, cfg.ItemsKey) == nil {
				records = nil
			} else {
				return false, fmt.Errorf("items_path %q is not an array", cfg.ItemsKey)
			}
		}

		for _, r := range records {
			rec, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if cfg.MaxItems > 0 && count >= cfg.MaxItems {
				return true, nil
			}
			count++
			stop, err := fn(rec)
			if err != nil {
				return false, err
			}
			if stop {
				return true, nil
			}
		}

		switch cfg.Pagination {
		case PageNone:
			next = ""
		case PageOffset, PagePage:
			if len(records) < cfg.PageSize {
				next = ""
			}
		case PageCursor:
			cursor = websource.Stringify(websource.Lookup(body, cfg.CursorPath))
			if cursor == "" || len(records) == 0 {
				next = ""
			}
		case PageLink:
			next = resolve(u, websource.NextLink(res.Header))
		case PageNext:
			next = resolve(u, websource.Stringify(websource.Lookup(body, cfg.NextPath)))
		}
		if next != "" && !websource.SameOrigin(cfg.URL, next) {
			return false, fmt.Errorf("next page URL changed origin")
		}
	}

	return false, nil
}

func resolve(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ""
	}

	return base.ResolveReference(r).String()
}

func formatSince(t time.Time, format string) string {
	switch format {
	case "unix":
		return strconv.FormatInt(t.Unix(), 10)
	case "unix_ms":
		return strconv.FormatInt(t.UnixMilli(), 10)
	default:
		return t.UTC().Format(format)
	}
}

func (f *Fetcher) request(ctx context.Context, cfg Config, endpoint string) (*websource.Response, any, error) {
	var body io.Reader
	if cfg.Method == http.MethodPost && cfg.Body != "" {
		body = strings.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(ctx, cfg.Method, endpoint, body)
	if err != nil {
		return nil, nil, fmt.Errorf("build request; %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	websource.ApplyAuth(req, cfg.User, cfg.Token)

	res, err := websource.Do(f.client, req, maxResponse)
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(res.Body)))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, nil, fmt.Errorf("decode response; %w", err)
	}

	return res, out, nil
}

// Preview counts the records the config selects.
func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var out websource.PreviewResult
	if err := f.Validate(raw); err != nil {
		return out, err
	}
	cfg, _ := decode(raw)
	out.Limit = cfg.MaxItems
	truncated, err := f.walk(ctx, cfg, time.Time{}, func(rec map[string]any) (bool, error) {
		out.Scanned++
		if websource.Stringify(websource.Lookup(rec, cfg.IDPath)) != "" {
			out.ItemCount++
		}
		return false, nil
	})
	out.Truncated = truncated

	return out, err
}

func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, rawState json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	tmpl, err := websource.CompileTemplate(cfg.Template)
	if err != nil {
		return nil, err
	}

	var state syncState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &state)
	}
	filter := filterSignature(cfg)
	full := cfg.SinceParam == "" || filter != state.Filter ||
		websource.FullResyncDue(state.FullAt, cfg.FullResyncSchedule, time.Now())
	since := state.Since
	if full {
		since = time.Time{}
	}
	startedAt := time.Now()
	maxSeen := since
	n := 0

	truncated, err := f.walk(ctx, cfg, since, func(rec map[string]any) (bool, error) {
		id := strings.TrimSpace(websource.Stringify(websource.Lookup(rec, cfg.IDPath)))
		if id == "" {
			return false, nil
		}
		page, err := renderRecord(cfg, tmpl, rec)
		if err != nil {
			page = websource.RemotePage{Err: err}
		}
		page.Slug = websource.ListingSlug(id)
		if page.UpdatedAt.After(maxSeen) {
			maxSeen = page.UpdatedAt
		}
		n++
		progress.Report(ctx, n, 0)

		return false, emit(page)
	})
	if err != nil {
		return nil, err
	}

	next := syncState{Since: maxSeen, FullAt: state.FullAt, Filter: filter}
	if maxSeen.IsZero() || cfg.UpdatedPath == "" {
		next.Since = startedAt
	}
	if full {
		next.FullAt = time.Now()
	}
	stateRaw, _ := json.Marshal(next)

	return &websource.FetchResult{Complete: full && !truncated, State: stateRaw}, nil
}

func renderRecord(cfg Config, tmpl *template.Template, rec map[string]any) (websource.RemotePage, error) {
	page := websource.RemotePage{
		Title:     strings.TrimSpace(websource.Stringify(websource.Lookup(rec, cfg.TitlePath))),
		UpdatedAt: websource.ParseTime(websource.Lookup(rec, cfg.UpdatedPath)),
	}
	if cfg.UpdatedPath == "" {
		page.UpdatedAt = time.Time{}
	}
	for _, p := range cfg.TeamPaths {
		page.Teams = append(page.Teams, websource.StringList(websource.Lookup(rec, p))...)
	}
	switch {
	case cfg.URLTemplate != "":
		page.URL, _ = websource.ExpandTemplate(cfg.URLTemplate, rec)
	case cfg.URLPath != "":
		page.URL = websource.Stringify(websource.Lookup(rec, cfg.URLPath))
	}
	if websource.ValidHTTPURL(page.URL) != nil {
		page.URL = ""
	}
	if page.Title == "" {
		page.Title = websource.Stringify(websource.Lookup(rec, cfg.IDPath))
	}

	if tmpl != nil {
		md, err := websource.ExecTemplate(tmpl, rec)
		page.Markdown = md

		return page, err
	}

	body := ""
	if cfg.BodyPath != "" {
		body = websource.Stringify(websource.Lookup(rec, cfg.BodyPath))
		if cfg.BodyFormat == "html" && body != "" {
			doc, err := convert.ToMarkdown([]byte(body), convert.Options{ContentType: "text/html", BaseURL: page.URL})
			if err != nil {
				return page, err
			}
			body = doc.Markdown
		}
	}
	skip := []string{cfg.TitlePath}
	meta := websource.RenderRecord(rec, "", append(skip, cfg.BodyPath)...)
	page.Markdown = strings.TrimSpace(meta + "\n" + body)

	return page, nil
}
