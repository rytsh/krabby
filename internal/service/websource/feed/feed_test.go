package feed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

const rss = `<?xml version="1.0"?><rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/"><channel><title>Blog</title>
<item><title>v1.2 released</title><link>https://example.com/v1.2</link><guid>r12</guid><pubDate>Mon, 05 Oct 2026 10:00:00 +0000</pubDate>
<content:encoded><![CDATA[<p>New <b>exporter</b></p>]]></content:encoded><category>release</category></item>
</channel></rss>`

const atom = `<?xml version="1.0" encoding="utf-8"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Status</title>
<entry><id>tag:x,1</id><title>Outage resolved</title><link rel="alternate" href="https://status.example.com/1"/>
<updated>2026-10-01T12:00:00Z</updated><summary>DB failover done</summary><author><name>SRE</name></author></entry></feed>`

func TestParse(t *testing.T) {
	title, entries, err := Parse([]byte(rss))
	if err != nil || title != "Blog" || len(entries) != 1 || entries[0].ID != "r12" || entries[0].Published.IsZero() {
		t.Fatalf("rss = %q %+v %v", title, entries, err)
	}
	title, entries, err = Parse([]byte(atom))
	if err != nil || title != "Status" || entries[0].Link != "https://status.example.com/1" || entries[0].Author != "SRE" {
		t.Fatalf("atom = %q %+v %v", title, entries, err)
	}
	if _, _, err := Parse([]byte("<html></html>")); err == nil {
		t.Fatal("html accepted as feed")
	}
}

func TestFetchKeepsEntriesThatLeaveTheFeed(t *testing.T) {
	body := rss
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()

	f := New()
	cfg, err := f.MergeConfig(nil, json.RawMessage(`{"urls":["`+srv.URL+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var pages []websource.RemotePage
	res, err := f.Fetch(context.Background(), &websource.Collection{Config: cfg}, nil, nil, func(p websource.RemotePage) error {
		pages = append(pages, p)
		return nil
	})
	if err != nil || len(pages) != 1 || !strings.Contains(pages[0].Markdown, "**exporter**") || !strings.Contains(pages[0].Markdown, "release") {
		t.Fatalf("pages = %+v %v", pages, err)
	}
	if res.Complete {
		t.Fatal("feed claims completeness without prune_missing")
	}

	body = strings.Replace(rss, "<item>", "<!--", 1)
	body = strings.Replace(body, "</item>", "-->", 1)
	res, err = f.Fetch(context.Background(), &websource.Collection{Config: cfg}, nil, res.State, func(websource.RemotePage) error { return nil })
	if err != nil || len(res.Removed) != 0 {
		t.Fatalf("removed = %v %v", res.Removed, err)
	}
}
