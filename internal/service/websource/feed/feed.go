// Package feed implements the "feed" web-source type: RSS 2.0, RSS 1.0 (RDF)
// and Atom feeds — blogs, release notes, changelogs, status pages. Each entry
// becomes a document; feeds only carry their latest entries, so entries are
// kept after they drop off the feed unless prune_missing is set, and entries
// older than max_age_days are removed.
package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/convert"
)

// Type is the collection type served by this fetcher.
const Type = "feed"

// Config is the provider config.
type Config struct {
	URLs         []string `json:"urls"`
	User         string   `json:"user,omitempty"`
	Token        string   `json:"token,omitempty"`
	FetchFull    bool     `json:"fetch_full_article,omitempty"`
	PruneMissing bool     `json:"prune_missing,omitempty"`
	MaxAgeDays   int      `json:"max_age_days,omitempty"`
	Teams        []string `json:"teams,omitempty"`
}

// Fetcher syncs feeds.
type Fetcher struct {
	websource.Schema
	client *http.Client
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: websource.DefaultHTTPClient(), Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "RSS / Atom feed",
		Description: "Index RSS or Atom feeds (blogs, release notes, changelogs, status pages). Entries stay indexed " +
			"after they leave the feed unless prune_missing is set.",
		Fields: []websource.Field{
			{Key: "urls", Label: "Feed URLs", Kind: websource.FieldList, Required: true, Placeholder: "https://github.com/org/repo/releases.atom"},
			{Key: "user", Label: "User (basic auth)", Kind: websource.FieldString},
			{Key: "token", Label: "Token / password", Kind: websource.FieldSecret},
			{Key: "fetch_full_article", Label: "Fetch the linked article for entries with only a summary", Kind: websource.FieldBool},
			{Key: "prune_missing", Label: "Remove entries that left the feed", Kind: websource.FieldBool},
			{Key: "max_age_days", Label: "Drop entries older than (days, 0 = keep)", Kind: websource.FieldNumber},
			{Key: "teams", Label: "Team tags for every entry", Kind: websource.FieldList},
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
	if len(cfg.URLs) == 0 {
		return fmt.Errorf("feed needs at least one URL")
	}
	for _, u := range cfg.URLs {
		if err := websource.ValidHTTPURL(u); err != nil {
			return fmt.Errorf("feed url %q %w", u, err)
		}
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

// Entry is one parsed feed item.
type Entry struct {
	ID        string
	Title     string
	Link      string
	Content   string // HTML or text
	Author    string
	Tags      []string
	Published time.Time
	Updated   time.Time
}

type xmlLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Text string `xml:",chardata"`
}

type rawFeed struct {
	XMLName xml.Name
	// RSS 2.0
	Channel struct {
		Title string    `xml:"title"`
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
	// RSS 1.0 (RDF) puts items at the root
	Items []rssItem `xml:"item"`
	// Atom
	Title   string      `xml:"title"`
	Entries []atomEntry `xml:"entry"`
}

type rssItem struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	GUID        string   `xml:"guid"`
	Description string   `xml:"description"`
	Encoded     string   `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	PubDate     string   `xml:"pubDate"`
	Date        string   `xml:"http://purl.org/dc/elements/1.1/ date"`
	Creator     string   `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Author      string   `xml:"author"`
	Categories  []string `xml:"category"`
}

type atomEntry struct {
	ID        string    `xml:"id"`
	Title     string    `xml:"title"`
	Links     []xmlLink `xml:"link"`
	Content   string    `xml:"content"`
	Summary   string    `xml:"summary"`
	Updated   string    `xml:"updated"`
	Published string    `xml:"published"`
	Author    struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Categories []struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
}

// Parse decodes an RSS 2.0, RSS 1.0 or Atom document.
func Parse(data []byte) (string, []Entry, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.CharsetReader = charset.NewReaderLabel
	d.Strict = false
	var raw rawFeed
	if err := d.Decode(&raw); err != nil {
		return "", nil, fmt.Errorf("parse feed; %w", err)
	}

	var out []Entry
	switch strings.ToLower(raw.XMLName.Local) {
	case "rss", "rdf":
		items := raw.Channel.Items
		if len(items) == 0 {
			items = raw.Items
		}
		for _, it := range items {
			e := Entry{
				ID: firstNonEmpty(it.GUID, it.Link, it.Title), Title: strings.TrimSpace(it.Title),
				Link: strings.TrimSpace(it.Link), Content: firstNonEmpty(it.Encoded, it.Description),
				Author: firstNonEmpty(it.Creator, it.Author), Tags: it.Categories,
				Published: websource.ParseTime(firstNonEmpty(it.PubDate, it.Date)),
			}
			e.Updated = e.Published
			out = append(out, e)
		}
		return firstNonEmpty(raw.Channel.Title, raw.Title), out, nil
	case "feed":
		for _, it := range raw.Entries {
			e := Entry{
				ID: strings.TrimSpace(it.ID), Title: strings.TrimSpace(it.Title), Content: firstNonEmpty(it.Content, it.Summary),
				Author: it.Author.Name, Published: websource.ParseTime(it.Published), Updated: websource.ParseTime(it.Updated),
			}
			for _, l := range it.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					e.Link = firstNonEmpty(l.Href, l.Text)
					break
				}
			}
			for _, c := range it.Categories {
				e.Tags = append(e.Tags, c.Term)
			}
			if e.ID == "" {
				e.ID = firstNonEmpty(e.Link, e.Title)
			}
			if e.Updated.IsZero() {
				e.Updated = e.Published
			}
			out = append(out, e)
		}
		return raw.Title, out, nil
	default:
		return "", nil, fmt.Errorf("not an RSS or Atom feed (root <%s>)", raw.XMLName.Local)
	}
}

func (f *Fetcher) get(ctx context.Context, cfg Config, u, accept string) (*websource.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	websource.ApplyAuth(req, cfg.User, cfg.Token)

	return websource.Do(f.client, req, 16<<20)
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var out websource.PreviewResult
	if err := f.Validate(raw); err != nil {
		return out, err
	}
	cfg, _ := decode(raw)
	for _, u := range cfg.URLs {
		res, err := f.get(ctx, cfg, u, "application/rss+xml, application/atom+xml, application/xml;q=0.9")
		if err != nil {
			return out, err
		}
		_, entries, err := Parse(res.Body)
		if err != nil {
			return out, fmt.Errorf("%s: %w", u, err)
		}
		out.Scanned += len(entries)
		out.ItemCount += len(entries)
	}

	return out, nil
}

type syncState struct {
	// Seen maps entry slugs to their last update, so entries that left the
	// feed can still be aged out.
	Seen map[string]time.Time `json:"seen"`
}

func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, rawState json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	var state syncState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &state)
	}
	if state.Seen == nil {
		state.Seen = map[string]time.Time{}
	}
	cutoff := time.Time{}
	if cfg.MaxAgeDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -cfg.MaxAgeDays)
	}

	current := map[string]bool{}
	for _, u := range cfg.URLs {
		res, err := f.get(ctx, cfg, u, "application/rss+xml, application/atom+xml, application/xml;q=0.9")
		if err != nil {
			return nil, err
		}
		feedTitle, entries, err := Parse(res.Body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u, err)
		}
		for _, e := range entries {
			if !cutoff.IsZero() && !e.Updated.IsZero() && e.Updated.Before(cutoff) {
				continue
			}
			slug := websource.ListingSlug(u + "\x00" + e.ID)
			current[slug] = true
			state.Seen[slug] = e.Updated
			if err := emit(f.render(ctx, cfg, feedTitle, e, slug)); err != nil {
				return nil, err
			}
		}
	}

	var removed []string
	for slug, updated := range state.Seen {
		if current[slug] {
			continue
		}
		if cfg.PruneMissing || (!cutoff.IsZero() && !updated.IsZero() && updated.Before(cutoff)) {
			removed = append(removed, slug)
			delete(state.Seen, slug)
		}
	}
	raw, _ := json.Marshal(state)

	return &websource.FetchResult{Complete: cfg.PruneMissing, Removed: removed, State: raw}, nil
}

func (f *Fetcher) render(ctx context.Context, cfg Config, feedTitle string, e Entry, slug string) websource.RemotePage {
	page := websource.RemotePage{Slug: slug, Title: e.Title, UpdatedAt: e.Updated, Teams: cfg.Teams}
	if websource.ValidHTTPURL(e.Link) == nil {
		page.URL = e.Link
	}
	body := e.Content
	if cfg.FetchFull && page.URL != "" && len(stripTags(body)) < 400 {
		if res, err := f.get(ctx, cfg, page.URL, "text/html"); err == nil {
			if doc, err := convert.ToMarkdown(res.Body, convert.Options{ContentType: res.Header.Get("Content-Type"), Name: res.URL, BaseURL: res.URL, Readability: true}); err == nil {
				body = ""
				page.Markdown = doc.Markdown
			}
		}
	}
	if body != "" {
		doc, err := convert.ToMarkdown([]byte(body), convert.Options{ContentType: "text/html", BaseURL: page.URL})
		if err != nil {
			page.Markdown = stripTags(body)
		} else {
			page.Markdown = doc.Markdown
		}
	}

	var meta strings.Builder
	if feedTitle != "" {
		fmt.Fprintf(&meta, "- **Feed**: %s\n", strings.TrimSpace(feedTitle))
	}
	if !e.Published.IsZero() {
		fmt.Fprintf(&meta, "- **Published**: %s\n", e.Published.UTC().Format("2006-01-02 15:04"))
	}
	if e.Author != "" {
		fmt.Fprintf(&meta, "- **Author**: %s\n", e.Author)
	}
	if len(e.Tags) > 0 {
		fmt.Fprintf(&meta, "- **Tags**: %s\n", strings.Join(e.Tags, ", "))
	}
	page.Markdown = strings.TrimSpace(meta.String() + "\n" + page.Markdown)
	if page.Title == "" {
		page.Title = firstNonEmpty(e.Link, e.ID)
	}

	return page
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}

	return strings.TrimSpace(b.String())
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
