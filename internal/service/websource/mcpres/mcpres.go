// Package mcpres implements the "mcp" web-source type: it indexes the
// resources an administrator granted on an external MCP connection (Notion,
// Drive, databases, … exposed by their MCP servers), so anything such a server
// publishes as a resource becomes searchable.
package mcpres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/convert"
)

// Type is the collection type served by this fetcher.
const Type = "mcp"

// Reader is the slice of the external-MCP store the fetcher needs.
type Reader interface {
	SyncResources(ctx context.Context, name string, include func(uri string) bool, fn func(mcpclient.ResourceDoc) error) error
}

// Config is the provider config.
type Config struct {
	Connection string   `json:"connection"`
	Include    []string `json:"include,omitempty"`
	Exclude    []string `json:"exclude,omitempty"`
}

// Fetcher syncs granted MCP resources.
type Fetcher struct {
	websource.Schema
	reader Reader
}

// New creates the fetcher over the external MCP store.
func New(reader Reader) *Fetcher {
	return &Fetcher{reader: reader, Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "External MCP resources",
		Description: "Index the resources granted on an external MCP connection (configured under External MCP). " +
			"Only exact resource URIs selected on the connection are read; tools are never called.",
		Fields: []websource.Field{
			{Key: "connection", Label: "External MCP connection name", Kind: websource.FieldString, Required: true, Placeholder: "notion"},
			{Key: "include", Label: "Include URI globs", Kind: websource.FieldList, Placeholder: "notion://pages/**"},
			{Key: "exclude", Label: "Exclude URI globs", Kind: websource.FieldList},
		},
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)
	cfg.Connection = strings.TrimSpace(cfg.Connection)

	return cfg, err
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if cfg.Connection == "" {
		return fmt.Errorf("mcp connection is required")
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

func filterOf(cfg Config) websource.KeyFilter {
	return websource.KeyFilter{Include: cfg.Include, Exclude: cfg.Exclude}
}

// uriKey strips the scheme so URI globs read like paths.
func uriKey(uri string) string {
	if _, rest, ok := strings.Cut(uri, "://"); ok {
		return rest
	}

	return uri
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var out websource.PreviewResult
	cfg, err := decode(raw)
	if err != nil {
		return out, err
	}
	filter := filterOf(cfg)
	err = f.reader.SyncResources(ctx, cfg.Connection, func(uri string) bool {
		out.Scanned++
		return filter.Match(uriKey(uri))
	}, func(doc mcpclient.ResourceDoc) error {
		if doc.Err == nil {
			out.ItemCount++
		}
		return nil
	})

	return out, err
}

// Fetch reads every granted resource. Resources carry no change token the
// server is obliged to keep, so every sync re-reads them; the manager's hash
// check keeps unchanged ones from being re-embedded. The sweep is complete, so
// revoked or unlisted resources are pruned.
func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, _ json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	filter := filterOf(cfg)
	err = f.reader.SyncResources(ctx, cfg.Connection, func(uri string) bool {
		return filter.Match(uriKey(uri))
	}, func(doc mcpclient.ResourceDoc) error {
		return emit(toPage(doc))
	})
	if err != nil {
		return nil, err
	}

	return &websource.FetchResult{Complete: true}, nil
}

func toPage(doc mcpclient.ResourceDoc) websource.RemotePage {
	res := doc.Resource
	page := websource.RemotePage{Slug: websource.ListingSlug(res.URI), Title: firstNonEmpty(res.Title, res.Name, uriKey(res.URI))}
	if websource.ValidHTTPURL(res.URI) == nil {
		page.URL = res.URI
	}
	if doc.Err != nil {
		page.Err = doc.Err

		return page
	}

	var parts []string
	for _, c := range doc.Contents {
		md, err := contentMarkdown(c, res)
		if err != nil {
			page.Err = err

			return page
		}
		if md != "" {
			parts = append(parts, md)
		}
	}
	if res.Description != "" {
		parts = append([]string{"> " + strings.ReplaceAll(strings.TrimSpace(res.Description), "\n", " ")}, parts...)
	}
	page.Markdown = strings.Join(parts, "\n\n")
	if strings.TrimSpace(page.Markdown) == "" {
		page.Err = fmt.Errorf("resource %s has no indexable content", res.URI)
	}

	return page
}

func contentMarkdown(c *mcp.ResourceContents, res *mcp.Resource) (string, error) {
	mimeType := firstNonEmpty(c.MIMEType, res.MIMEType)
	data := []byte(c.Text)
	if len(c.Blob) > 0 {
		data = c.Blob
	}
	if len(data) == 0 {
		return "", nil
	}
	if mimeType == "" && len(c.Blob) == 0 {
		mimeType = "text/plain"
	}
	doc, err := convert.ToMarkdown(data, convert.Options{ContentType: mimeType, Name: firstNonEmpty(res.Name, c.URI)})
	if err != nil {
		return "", err
	}

	return doc.Markdown, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
