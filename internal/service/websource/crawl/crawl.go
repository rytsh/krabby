// Package crawl implements the "crawl" web-source type: starting from one or
// more URLs it follows same-origin links breadth-first up to a depth and page
// limit, honours robots.txt, filters URLs with include/exclude globs and
// converts every page (HTML, and linked PDF / Office / text documents) to
// Markdown. Conditional requests (ETag / Last-Modified) skip unchanged pages.
// Documentation sites (Docusaurus, MkDocs, GitBook, Sphinx) are the main use.
package crawl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/convert"
)

// Type is the collection type served by this fetcher.
const Type = "crawl"

const (
	defaultMaxPages = 500
	hardMaxPages    = 20_000
	defaultDepth    = 3
	maxBodyBytes    = 16 << 20
)

// Config is the provider config.
type Config struct {
	StartURLs      []string `json:"start_urls"`
	MaxDepth       *int     `json:"max_depth,omitempty"`
	MaxPages       int      `json:"max_pages,omitempty"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	PathPrefix     string   `json:"path_prefix,omitempty"`
	IgnoreRobots   bool     `json:"ignore_robots,omitempty"`
	FollowDocs     *bool    `json:"follow_documents,omitempty"`
	DelayMS        int      `json:"delay_ms,omitempty"`
	User           string   `json:"user,omitempty"`
	Token          string   `json:"token,omitempty"`
	UserAgent      string   `json:"user_agent,omitempty"`
	ReadabilityOff bool     `json:"readability_off,omitempty"`
}

func (c Config) depth() int {
	if c.MaxDepth == nil {
		return defaultDepth
	}

	return max(*c.MaxDepth, 0)
}

func (c Config) maxPages() int {
	if c.MaxPages <= 0 {
		return defaultMaxPages
	}

	return min(c.MaxPages, hardMaxPages)
}

func (c Config) followDocs() bool { return c.FollowDocs == nil || *c.FollowDocs }

// Fetcher crawls sites.
type Fetcher struct {
	websource.Schema
	client *http.Client
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: websource.DefaultHTTPClient(), Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "Web crawler (docs site)",
		Description: "Crawl a documentation site from start URLs: same-origin links only, depth and page limits, " +
			"robots.txt, include/exclude URL globs, conditional requests to skip unchanged pages. Linked PDF/Office files are converted too.",
		Fields: []websource.Field{
			{Key: "start_urls", Label: "Start URLs", Kind: websource.FieldList, Required: true, Placeholder: "https://docs.example.com/"},
			{Key: "max_depth", Label: "Max link depth", Kind: websource.FieldNumber, Default: defaultDepth},
			{Key: "max_pages", Label: "Max pages", Kind: websource.FieldNumber, Default: defaultMaxPages},
			{Key: "path_prefix", Label: "Stay under path prefix", Kind: websource.FieldString, Placeholder: "/docs/"},
			{Key: "include", Label: "Include path globs", Kind: websource.FieldList, Placeholder: "docs/**"},
			{Key: "exclude", Label: "Exclude path globs", Kind: websource.FieldList, Placeholder: "**/changelog/**, blog/**"},
			{Key: "follow_documents", Label: "Also index linked PDF / Office / text files", Kind: websource.FieldBool, Default: true},
			{Key: "ignore_robots", Label: "Ignore robots.txt", Kind: websource.FieldBool},
			{Key: "delay_ms", Label: "Delay between requests (ms)", Kind: websource.FieldNumber},
			{Key: "user", Label: "User (basic auth)", Kind: websource.FieldString},
			{Key: "token", Label: "Token / password", Kind: websource.FieldSecret},
			{Key: "user_agent", Label: "User-Agent", Kind: websource.FieldString, Placeholder: "krabby-crawler/1.0"},
			{Key: "readability_off", Label: "Index full HTML (disable article extraction)", Kind: websource.FieldBool},
		},
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	return websource.DecodeConfig[Config](raw, Type)
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if len(cfg.StartURLs) == 0 {
		return fmt.Errorf("crawl needs at least one start URL")
	}
	for _, u := range cfg.StartURLs {
		if err := websource.ValidHTTPURL(u); err != nil {
			return fmt.Errorf("start url %q %w", u, err)
		}
	}
	if cfg.DelayMS < 0 || cfg.DelayMS > 60_000 {
		return fmt.Errorf("delay_ms must be between 0 and 60000")
	}

	return nil
}

func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	merged, err := f.MergeObjects(current, update)
	if err != nil {
		return nil, err
	}

	return merged, f.Validate(merged)
}

func (f *Fetcher) ConfigView(raw json.RawMessage) any { return f.View(raw) }

// pageState is the conditional-request cache of one URL.
type pageState struct {
	ETag         string   `json:"e,omitempty"`
	LastModified string   `json:"m,omitempty"`
	Links        []string `json:"l,omitempty"`
}

type syncState struct {
	Pages  map[string]pageState `json:"pages"`
	Filter string               `json:"filter,omitempty"`
}

func filterSignature(cfg Config) string {
	return websource.Hash(fmt.Sprint(cfg.StartURLs, cfg.depth(), cfg.maxPages(), cfg.Include, cfg.Exclude,
		cfg.PathPrefix, cfg.IgnoreRobots, cfg.followDocs(), cfg.ReadabilityOff))
}

// crawler holds one run's state.
type crawler struct {
	f       *Fetcher
	cfg     Config
	filter  websource.KeyFilter
	robots  map[string]*robots
	prev    map[string]pageState
	next    map[string]pageState
	origins map[string]bool
	ua      string
}

type queued struct {
	url   string
	depth int
}

// Fetch crawls the site. Pages answering 304 Not Modified are not emitted;
// their cached outgoing links are still followed, so the crawl frontier is
// the same as a full run. A crawl that reached its page cap is incomplete and
// prunes nothing; otherwise pages no longer reachable are reported removed.
func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, rawState json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	var prev syncState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &prev)
	}
	filter := filterSignature(cfg)
	if prev.Filter != filter {
		prev.Pages = nil
	}

	c := &crawler{
		f: f, cfg: cfg, filter: websource.KeyFilter{Include: cfg.Include, Exclude: cfg.Exclude},
		robots: map[string]*robots{}, prev: prev.Pages, next: map[string]pageState{}, origins: map[string]bool{},
		ua: firstNonEmpty(cfg.UserAgent, "krabby-crawler/1.0"),
	}
	queue := make([]queued, 0, len(cfg.StartURLs))
	seen := map[string]bool{}
	for _, s := range cfg.StartURLs {
		u := normalize(s)
		if u == "" {
			continue
		}
		parsed, _ := url.Parse(u)
		c.origins[parsed.Scheme+"://"+parsed.Host] = true
		if !seen[u] {
			seen[u] = true
			queue = append(queue, queued{url: u})
		}
	}

	visited := 0
	truncated := false
	allEmitted := true

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if visited >= cfg.maxPages() {
			truncated = true

			break
		}
		item := queue[0]
		queue = queue[1:]

		if !c.allowed(ctx, item.url) {
			continue
		}
		visited++
		if visited > 1 && cfg.DelayMS > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(cfg.DelayMS) * time.Millisecond):
			}
		}

		page, links, notModified, err := c.fetch(ctx, item.url)
		switch {
		case err != nil && errors.Is(err, errSkip):
			// Not indexable (redirected off-site, unsupported type): drop it.
			continue
		case err != nil:
			if old, ok := c.prev[item.url]; ok {
				c.next[item.url] = pageState{Links: old.Links}
			}
			if emitErr := emit(websource.RemotePage{Slug: slugFor(item.url), URL: item.url, Err: err}); emitErr != nil {
				return nil, emitErr
			}
		case notModified:
			allEmitted = false
		default:
			if emitErr := emit(page); emitErr != nil {
				return nil, emitErr
			}
		}
		progress.Report(ctx, visited, min(visited+len(queue), cfg.maxPages()))

		if item.depth >= cfg.depth() {
			continue
		}
		for _, link := range links {
			if seen[link] || !c.inScope(link) {
				continue
			}
			seen[link] = true
			queue = append(queue, queued{url: link, depth: item.depth + 1})
		}
	}

	var removed []string
	if !truncated {
		for u := range c.prev {
			if _, ok := c.next[u]; !ok {
				removed = append(removed, slugFor(u))
			}
		}
	}
	state, _ := json.Marshal(syncState{Pages: c.next, Filter: filter})

	return &websource.FetchResult{Complete: !truncated && allEmitted, Removed: removed, State: state}, nil
}

// Preview crawls without converting and reports how many pages the config
// reaches (bounded by max_pages).
func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var out websource.PreviewResult
	if err := f.Validate(raw); err != nil {
		return out, err
	}
	cfg, _ := decode(raw)
	if cfg.MaxPages <= 0 || cfg.MaxPages > 200 {
		cfg.MaxPages = 200
	}
	out.Limit = cfg.MaxPages
	col := &websource.Collection{Config: mustJSON(cfg)}
	res, err := f.Fetch(ctx, col, nil, nil, func(p websource.RemotePage) error {
		out.Scanned++
		if p.Err == nil {
			out.ItemCount++
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	out.Truncated = !res.Complete

	return out, nil
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)

	return raw
}

var errSkip = errors.New("skip")

func slugFor(u string) string { return websource.ListingSlug(u) }

// normalize drops fragments and default ports so one page has one key.
func normalize(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	u.RawFragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Host = strings.TrimSuffix(strings.TrimSuffix(u.Host, ":80"), ":443")
	if u.Path == "" {
		u.Path = "/"
	}

	return u.String()
}

func (c *crawler) inScope(link string) bool {
	u, err := url.Parse(link)
	if err != nil || !c.origins[u.Scheme+"://"+u.Host] {
		return false
	}
	if c.cfg.PathPrefix != "" && !strings.HasPrefix(u.Path, c.cfg.PathPrefix) {
		return false
	}
	if !c.cfg.followDocs() {
		ext := strings.ToLower(path.Ext(u.Path))
		if ext != "" && ext != ".html" && ext != ".htm" && ext != ".php" && ext != ".aspx" {
			return false
		}
	}
	if skipExt[strings.ToLower(path.Ext(u.Path))] {
		return false
	}

	return c.filter.Match(strings.TrimPrefix(u.Path, "/"))
}

// skipExt are assets that are never documents.
var skipExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".webp": true, ".ico": true,
	".css": true, ".js": true, ".mjs": true, ".map": true, ".woff": true, ".woff2": true, ".ttf": true,
	".zip": true, ".gz": true, ".tar": true, ".mp4": true, ".mp3": true, ".webm": true, ".exe": true, ".dmg": true,
}

func (c *crawler) allowed(ctx context.Context, raw string) bool {
	if c.cfg.IgnoreRobots {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	origin := u.Scheme + "://" + u.Host
	r, ok := c.robots[origin]
	if !ok {
		r = c.loadRobots(ctx, origin)
		c.robots[origin] = r
	}

	return r.allowed(u.EscapedPath())
}

func (c *crawler) loadRobots(ctx context.Context, origin string) *robots {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return &robots{}
	}
	req.Header.Set("User-Agent", c.ua)
	res, err := c.f.client.Do(req)
	if err != nil {
		return &robots{}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return &robots{}
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512<<10))

	return parseRobots(string(body), c.ua)
}

func (c *crawler) fetch(ctx context.Context, pageURL string) (websource.RemotePage, []string, bool, error) {
	page := websource.RemotePage{Slug: slugFor(pageURL), URL: pageURL}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return page, nil, false, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,text/plain,*/*;q=0.5")
	websource.ApplyAuth(req, c.cfg.User, c.cfg.Token)
	old, hadOld := c.prev[pageURL]
	if hadOld {
		if old.ETag != "" {
			req.Header.Set("If-None-Match", old.ETag)
		}
		if old.LastModified != "" {
			req.Header.Set("If-Modified-Since", old.LastModified)
		}
	}

	client := *c.f.client
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !c.origins[r.URL.Scheme+"://"+r.URL.Host] {
			return errSkip
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errSkip) {
			return page, nil, false, errSkip
		}
		return page, nil, false, fmt.Errorf("fetch %s; %w", pageURL, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusNotModified && hadOld {
		c.next[pageURL] = old

		return page, old.Links, true, nil
	}
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone {
		return page, nil, false, errSkip
	}
	if res.StatusCode != http.StatusOK {
		return page, nil, false, fmt.Errorf("fetch %s; status %s", pageURL, res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes+1))
	if err != nil {
		return page, nil, false, fmt.Errorf("read %s; %w", pageURL, err)
	}
	if len(body) > maxBodyBytes {
		return page, nil, false, errSkip
	}
	finalURL := pageURL
	if res.Request != nil && res.Request.URL != nil {
		finalURL = res.Request.URL.String()
	}

	contentType := res.Header.Get("Content-Type")
	format := convert.Detect(contentType, finalURL)
	if format == convert.FormatUnknown {
		return page, nil, false, errSkip
	}

	var links []string
	if format == convert.FormatHTML {
		if noIndex(body) {
			c.next[pageURL] = pageState{Links: extractLinks(body, finalURL)}
			return page, c.next[pageURL].Links, true, nil
		}
		links = extractLinks(body, finalURL)
	}
	doc, err := convert.ToMarkdown(body, convert.Options{
		ContentType: contentType, Name: finalURL, BaseURL: finalURL, Readability: !c.cfg.ReadabilityOff,
	})
	if err != nil {
		return page, links, false, err
	}
	page.Title = doc.Title
	page.Markdown = doc.Markdown
	if t, err := http.ParseTime(res.Header.Get("Last-Modified")); err == nil {
		page.UpdatedAt = t
	}
	c.next[pageURL] = pageState{ETag: res.Header.Get("ETag"), LastModified: res.Header.Get("Last-Modified"), Links: links}

	return page, links, false, nil
}

// noIndex reports a <meta name="robots" content="noindex">.
func noIndex(body []byte) bool {
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return false
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) == "body" {
				return false
			}
			if string(name) != "meta" || !hasAttr {
				continue
			}
			var metaName, content string
			for {
				k, v, more := z.TagAttr()
				switch strings.ToLower(string(k)) {
				case "name":
					metaName = strings.ToLower(string(v))
				case "content":
					content = strings.ToLower(string(v))
				}
				if !more {
					break
				}
			}
			if metaName == "robots" && strings.Contains(content, "noindex") {
				return true
			}
		}
	}
}

// extractLinks returns the normalized absolute href targets of a page,
// honouring <base href>.
func extractLinks(body []byte, pageURL string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return out
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		tag := string(name)
		if (tag != "a" && tag != "base" && tag != "link") || !hasAttr {
			continue
		}
		var href, rel string
		for {
			k, v, more := z.TagAttr()
			switch string(k) {
			case "href":
				href = string(v)
			case "rel":
				rel = strings.ToLower(string(v))
			}
			if !more {
				break
			}
		}
		if href == "" {
			continue
		}
		ref, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			continue
		}
		if tag == "base" {
			base = base.ResolveReference(ref)
			continue
		}
		if tag == "link" && rel != "next" && rel != "alternate" {
			continue
		}
		if strings.Contains(rel, "nofollow") {
			continue
		}
		if u := normalize(base.ResolveReference(ref).String()); u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
