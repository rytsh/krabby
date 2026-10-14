package mcpres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/service/websource"
)

type fakeReader struct{ docs []mcpclient.ResourceDoc }

func (r fakeReader) SyncResources(_ context.Context, name string, include func(string) bool, fn func(mcpclient.ResourceDoc) error) error {
	if name != "notion" {
		return errors.New("unknown connection")
	}
	for _, d := range r.docs {
		if include(d.Resource.URI) {
			if err := fn(d); err != nil {
				return err
			}
		}
	}

	return nil
}

func TestFetchGrantedResources(t *testing.T) {
	f := New(fakeReader{docs: []mcpclient.ResourceDoc{
		{Resource: &mcp.Resource{URI: "notion://pages/a", Name: "a", Title: "Onboarding"},
			Contents: []*mcp.ResourceContents{{MIMEType: "text/markdown", Text: "# Onboarding\n\nwelcome"}}},
		{Resource: &mcp.Resource{URI: "notion://pages/b.html", Name: "b"},
			Contents: []*mcp.ResourceContents{{MIMEType: "text/html", Text: "<h2>Billing</h2><p>invoices</p>"}}},
		{Resource: &mcp.Resource{URI: "notion://private/x"}, Contents: []*mcp.ResourceContents{{Text: "secret"}}},
		{Resource: &mcp.Resource{URI: "notion://pages/broken"}, Err: errors.New("read failed")},
	}})
	cfg, err := f.MergeConfig(nil, json.RawMessage(`{"connection":"notion","include":["pages/**"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var pages []websource.RemotePage
	res, err := f.Fetch(context.Background(), &websource.Collection{Config: cfg}, nil, nil, func(p websource.RemotePage) error {
		pages = append(pages, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(pages) != 3 {
		t.Fatalf("complete=%v pages=%+v", res.Complete, pages)
	}
	if pages[0].Title != "Onboarding" || !strings.Contains(pages[1].Markdown, "## Billing") || pages[2].Err == nil {
		t.Fatalf("pages = %+v", pages)
	}
	for _, p := range pages {
		if !websource.ValidSlug(p.Slug) {
			t.Fatalf("invalid slug %q", p.Slug)
		}
	}
}
