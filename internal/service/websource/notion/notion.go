// Package notion implements the "notion" web-source type: pages shared with a
// Notion integration (optionally limited to one database or a page subtree),
// rendered block by block to Markdown and synced incrementally by
// last_edited_time.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
)

// Type is the collection type served by this fetcher.
const Type = "notion"

const (
	apiVersion = "2022-06-28"
	maxDepth   = 6
	maxBlocks  = 5000
)

// Config is the provider config.
type Config struct {
	Token      string `json:"token"`
	DatabaseID string `json:"database_id,omitempty"`
	RootPageID string `json:"root_page_id,omitempty"`
	MaxPages   int    `json:"max_pages,omitempty"`
}

// Fetcher syncs Notion pages.
type Fetcher struct {
	websource.Schema
	client *http.Client
	api    string
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: websource.DefaultHTTPClient(), api: "https://api.notion.com/v1", Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "Notion workspace",
		Description: "Pages shared with a Notion internal integration, optionally limited to one database or a page subtree. " +
			"Unchanged pages are skipped by last_edited_time.",
		Fields: []websource.Field{
			{Key: "token", Label: "Integration secret", Kind: websource.FieldSecret, Required: true, Help: "Share the pages/databases with the integration in Notion"},
			{Key: "database_id", Label: "Database id (optional)", Kind: websource.FieldString},
			{Key: "root_page_id", Label: "Root page id (optional, page + child pages)", Kind: websource.FieldString},
			{Key: "max_pages", Label: "Max pages (0 = no limit)", Kind: websource.FieldNumber},
		},
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)
	cfg.DatabaseID = cleanID(cfg.DatabaseID)
	cfg.RootPageID = cleanID(cfg.RootPageID)

	return cfg, err
}

// cleanID accepts a bare id or a Notion URL and returns the 32-hex id.
func cleanID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s, _, _ = strings.Cut(s, "?")
	if i := strings.LastIndexAny(s, "/-"); i >= 0 && len(s)-i-1 == 32 {
		s = s[i+1:]
	}

	return strings.ReplaceAll(s, "-", "")
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		return fmt.Errorf("notion token is required")
	}
	if cfg.DatabaseID != "" && cfg.RootPageID != "" {
		return fmt.Errorf("set database_id or root_page_id, not both")
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

func (f *Fetcher) do(ctx context.Context, cfg Config, method, path string, body any, out any) error {
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.api+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Notion-Version", apiVersion)
	req.Header.Set("Content-Type", "application/json")
	for attempt := 0; ; attempt++ {
		res, err := websource.Do(f.client, req, 32<<20)
		if err != nil {
			if attempt < 3 && strings.Contains(err.Error(), "429") {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(attempt+1) * time.Second):
				}
				_, _ = rd.Seek(0, 0)
				continue
			}
			return err
		}
		return json.Unmarshal(res.Body, out)
	}
}

type pageObj struct {
	ID             string                     `json:"id"`
	URL            string                     `json:"url"`
	LastEditedTime time.Time                  `json:"last_edited_time"`
	Archived       bool                       `json:"archived"`
	InTrash        bool                       `json:"in_trash"`
	Properties     map[string]json.RawMessage `json:"properties"`
	Object         string                     `json:"object"`
}

type listResp struct {
	Results    []json.RawMessage `json:"results"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// listPages returns the selected pages' metadata.
func (f *Fetcher) listPages(ctx context.Context, cfg Config) ([]pageObj, error) {
	var out []pageObj
	add := func(raw json.RawMessage) {
		var p pageObj
		if json.Unmarshal(raw, &p) == nil && p.Object == "page" && !p.Archived && !p.InTrash {
			out = append(out, p)
		}
	}
	limitHit := func() bool { return cfg.MaxPages > 0 && len(out) >= cfg.MaxPages }

	switch {
	case cfg.DatabaseID != "":
		cursor := ""
		for {
			body := map[string]any{"page_size": 100}
			if cursor != "" {
				body["start_cursor"] = cursor
			}
			var r listResp
			if err := f.do(ctx, cfg, http.MethodPost, "/databases/"+cfg.DatabaseID+"/query", body, &r); err != nil {
				return nil, err
			}
			for _, raw := range r.Results {
				add(raw)
			}
			if !r.HasMore || limitHit() {
				break
			}
			cursor = r.NextCursor
		}
	case cfg.RootPageID != "":
		queue := []string{cfg.RootPageID}
		seen := map[string]bool{}
		for len(queue) > 0 && !limitHit() {
			id := queue[0]
			queue = queue[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			var p pageObj
			if err := f.do(ctx, cfg, http.MethodGet, "/pages/"+id, nil, &p); err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(p)
			add(raw)
			children, err := f.blocks(ctx, cfg, id)
			if err != nil {
				return nil, err
			}
			for _, b := range children {
				if b.Type == "child_page" {
					queue = append(queue, cleanID(b.ID))
				}
			}
		}
	default:
		cursor := ""
		for {
			body := map[string]any{"page_size": 100, "filter": map[string]string{"property": "object", "value": "page"}}
			if cursor != "" {
				body["start_cursor"] = cursor
			}
			var r listResp
			if err := f.do(ctx, cfg, http.MethodPost, "/search", body, &r); err != nil {
				return nil, err
			}
			for _, raw := range r.Results {
				add(raw)
			}
			if !r.HasMore || limitHit() {
				break
			}
			cursor = r.NextCursor
		}
	}
	if cfg.MaxPages > 0 && len(out) > cfg.MaxPages {
		out = out[:cfg.MaxPages]
	}

	return out, nil
}

// block is one Notion block; the type-specific payload sits under a key named
// after the type.
type block struct {
	ID          string                     `json:"id"`
	Type        string                     `json:"type"`
	HasChildren bool                       `json:"has_children"`
	Raw         map[string]json.RawMessage `json:"-"`
}

func (b *block) UnmarshalJSON(data []byte) error {
	type alias block
	if err := json.Unmarshal(data, (*alias)(b)); err != nil {
		return err
	}

	return json.Unmarshal(data, &b.Raw)
}

func (f *Fetcher) blocks(ctx context.Context, cfg Config, id string) ([]block, error) {
	var out []block
	cursor := ""
	for {
		path := "/blocks/" + id + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + cursor
		}
		var r struct {
			Results    []block `json:"results"`
			HasMore    bool    `json:"has_more"`
			NextCursor string  `json:"next_cursor"`
		}
		if err := f.do(ctx, cfg, http.MethodGet, path, nil, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Results...)
		if !r.HasMore || len(out) >= maxBlocks {
			return out, nil
		}
		cursor = r.NextCursor
	}
}

type richText struct {
	PlainText   string `json:"plain_text"`
	Href        string `json:"href"`
	Annotations struct {
		Bold          bool `json:"bold"`
		Italic        bool `json:"italic"`
		Code          bool `json:"code"`
		Strikethrough bool `json:"strikethrough"`
	} `json:"annotations"`
}

func renderRich(rts []richText) string {
	var b strings.Builder
	for _, rt := range rts {
		s := rt.PlainText
		if s == "" {
			continue
		}
		a := rt.Annotations
		switch {
		case a.Code:
			s = "`" + s + "`"
		default:
			if a.Bold {
				s = "**" + s + "**"
			}
			if a.Italic {
				s = "_" + s + "_"
			}
			if a.Strikethrough {
				s = "~~" + s + "~~"
			}
		}
		if rt.Href != "" {
			s = "[" + s + "](" + rt.Href + ")"
		}
		b.WriteString(s)
	}

	return b.String()
}

type textPayload struct {
	RichText []richText   `json:"rich_text"`
	Checked  bool         `json:"checked"`
	Language string       `json:"language"`
	Title    string       `json:"title"`
	URL      string       `json:"url"`
	Caption  []richText   `json:"caption"`
	Cells    [][]richText `json:"cells"`
	External struct {
		URL string `json:"url"`
	} `json:"external"`
	File struct {
		URL string `json:"url"`
	} `json:"file"`
}

func (f *Fetcher) renderBlocks(ctx context.Context, cfg Config, id string, depth int, budget *int, b *strings.Builder) error {
	if depth > maxDepth || *budget <= 0 {
		return nil
	}
	blocks, err := f.blocks(ctx, cfg, id)
	if err != nil {
		return err
	}
	indent := strings.Repeat("  ", max(depth, 0))
	num := 0
	for _, bl := range blocks {
		*budget--
		var p textPayload
		if raw, ok := bl.Raw[bl.Type]; ok {
			_ = json.Unmarshal(raw, &p)
		}
		text := renderRich(p.RichText)
		if bl.Type != "numbered_list_item" {
			num = 0
		}
		childDepth := depth
		switch bl.Type {
		case "heading_1":
			b.WriteString("\n## " + text + "\n\n")
		case "heading_2":
			b.WriteString("\n### " + text + "\n\n")
		case "heading_3":
			b.WriteString("\n#### " + text + "\n\n")
		case "paragraph":
			if text != "" {
				b.WriteString(indent + text + "\n\n")
			}
		case "bulleted_list_item", "toggle":
			b.WriteString(indent + "- " + text + "\n")
			childDepth = depth + 1
		case "numbered_list_item":
			num++
			fmt.Fprintf(b, "%s%d. %s\n", indent, num, text)
			childDepth = depth + 1
		case "to_do":
			mark := " "
			if p.Checked {
				mark = "x"
			}
			b.WriteString(indent + "- [" + mark + "] " + text + "\n")
			childDepth = depth + 1
		case "quote", "callout":
			b.WriteString("> " + strings.ReplaceAll(text, "\n", "\n> ") + "\n\n")
		case "code":
			b.WriteString("```" + p.Language + "\n" + plain(p.RichText) + "\n```\n\n")
		case "divider":
			b.WriteString("---\n\n")
		case "child_page":
			b.WriteString(indent + "- Child page: " + p.Title + "\n")
			continue // child pages are separate documents
		case "child_database":
			b.WriteString(indent + "- Database: " + p.Title + "\n")
			continue
		case "bookmark", "link_preview", "embed":
			if p.URL != "" {
				b.WriteString(indent + "- <" + p.URL + ">\n")
			}
		case "image":
			u := firstNonEmpty(p.External.URL, p.File.URL)
			if u != "" {
				fmt.Fprintf(b, "![%s](%s)\n\n", plain(p.Caption), u)
			}
		case "table_row":
			cells := make([]string, len(p.Cells))
			for i, c := range p.Cells {
				cells[i] = strings.ReplaceAll(renderRich(c), "|", `\|`)
			}
			b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		case "table":
			if err := f.renderTable(ctx, cfg, bl.ID, budget, b); err != nil {
				return err
			}
			continue
		}
		if bl.HasChildren {
			if err := f.renderBlocks(ctx, cfg, bl.ID, childDepth, budget, b); err != nil {
				return err
			}
		}
	}

	return nil
}

func (f *Fetcher) renderTable(ctx context.Context, cfg Config, id string, budget *int, b *strings.Builder) error {
	var rows strings.Builder
	if err := f.renderBlocks(ctx, cfg, id, 0, budget, &rows); err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(rows.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil
	}
	cols := strings.Count(lines[0], " | ") + 1
	b.WriteString("\n" + lines[0] + "\n|" + strings.Repeat(" --- |", cols) + "\n")
	for _, l := range lines[1:] {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n")

	return nil
}

func plain(rts []richText) string {
	var b strings.Builder
	for _, rt := range rts {
		b.WriteString(rt.PlainText)
	}

	return b.String()
}

// pageTitle reads the title-typed property and renders the remaining simple
// properties (database columns) as metadata.
func pageTitle(p pageObj) (string, string) {
	title := ""
	var meta strings.Builder
	for name, raw := range p.Properties {
		var prop struct {
			Type        string                  `json:"type"`
			Title       []richText              `json:"title"`
			RichText    []richText              `json:"rich_text"`
			Select      *struct{ Name string }  `json:"select"`
			Status      *struct{ Name string }  `json:"status"`
			MultiSelect []struct{ Name string } `json:"multi_select"`
			Number      *float64                `json:"number"`
			Date        *struct{ Start string } `json:"date"`
			URL         string                  `json:"url"`
			People      []struct{ Name string } `json:"people"`
		}
		if json.Unmarshal(raw, &prop) != nil {
			continue
		}
		val := ""
		switch prop.Type {
		case "title":
			title = plain(prop.Title)
			continue
		case "rich_text":
			val = plain(prop.RichText)
		case "select":
			if prop.Select != nil {
				val = prop.Select.Name
			}
		case "status":
			if prop.Status != nil {
				val = prop.Status.Name
			}
		case "multi_select":
			names := make([]string, 0, len(prop.MultiSelect))
			for _, s := range prop.MultiSelect {
				names = append(names, s.Name)
			}
			val = strings.Join(names, ", ")
		case "number":
			if prop.Number != nil {
				val = fmt.Sprint(*prop.Number)
			}
		case "date":
			if prop.Date != nil {
				val = prop.Date.Start
			}
		case "url":
			val = prop.URL
		case "people":
			names := make([]string, 0, len(prop.People))
			for _, s := range prop.People {
				names = append(names, s.Name)
			}
			val = strings.Join(names, ", ")
		}
		if strings.TrimSpace(val) != "" {
			fmt.Fprintf(&meta, "- **%s**: %s\n", name, val)
		}
	}

	return title, meta.String()
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	if err := f.Validate(raw); err != nil {
		return websource.PreviewResult{}, err
	}
	cfg, _ := decode(raw)
	pages, err := f.listPages(ctx, cfg)

	return websource.PreviewResult{ItemCount: len(pages), Scanned: len(pages), Limit: cfg.MaxPages}, err
}

func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, state json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	pages, err := f.listPages(ctx, cfg)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]pageObj, len(pages))
	items := make([]websource.ListedItem, 0, len(pages))
	for _, p := range pages {
		title, _ := pageTitle(p)
		byID[p.ID] = p
		items = append(items, websource.ListedItem{
			Key: p.ID, Version: p.LastEditedTime.UTC().Format(time.RFC3339Nano), Title: firstNonEmpty(title, "Untitled"),
			URL: p.URL, UpdatedAt: p.LastEditedTime,
		})
	}
	signature := websource.Hash(cfg.DatabaseID + "\x00" + cfg.RootPageID)

	res, err := websource.ListingSync(ctx, state, signature, items, func(ctx context.Context, item websource.ListedItem) (websource.RemotePage, error) {
		_, meta := pageTitle(byID[item.Key])
		var b strings.Builder
		budget := maxBlocks
		if err := f.renderBlocks(ctx, cfg, cleanID(item.Key), 0, &budget, &b); err != nil {
			return websource.RemotePage{}, err
		}
		md := strings.TrimSpace(meta + "\n" + b.String())

		return websource.RemotePage{Markdown: md}, nil
	}, emit)
	if err != nil {
		return nil, err
	}
	if cfg.MaxPages > 0 && len(pages) >= cfg.MaxPages {
		// A capped listing cannot tell removed pages from pages past the cap.
		res.Complete, res.Removed = false, nil
	}

	return res, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
