package crawl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestRobots(t *testing.T) {
	r := parseRobots("User-agent: *\nDisallow: /private/\nAllow: /private/ok\nDisallow: /*.json$\n\nUser-agent: other\nDisallow: /\n", "krabby-crawler/1.0")
	for p, want := range map[string]bool{
		"/docs": true, "/private/x": false, "/private/ok/page": true, "/a/b.json": false, "/a/b.json5": true,
	} {
		if got := r.allowed(p); got != want {
			t.Errorf("allowed(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestCrawlFollowsLinksWithinScope(t *testing.T) {
	var srv *httptest.Server
	hits := map[string]int{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/robots.txt":
			fmt.Fprint(w, "User-agent: *\nDisallow: /docs/secret\n") //nolint:errcheck // Test response write failures are surfaced by the client assertions.
		case "/docs/":
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Type", "text/html")
			//nolint:errcheck // Test response write failures are surfaced by the client assertions.
			fmt.Fprintf(w, `<html><head><title>Home</title></head><body><main><h1>Home</h1><p>welcome text for the docs home page</p>
<a href="guide">Guide</a><a href="/docs/secret">S</a><a href="/blog/">Blog</a><a href="https://other.example/">x</a>
<a href="manual.txt">txt</a><a href="logo.png">img</a></main></body></html>`)
		case "/docs/guide":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><title>Guide</title></head><body><main><h1>Guide</h1><p>install steps described here in detail</p><a href="/docs/">home</a></main></body></html>`) //nolint:errcheck // Test response write failures are surfaced by the client assertions.
		case "/docs/manual.txt":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "plain manual") //nolint:errcheck // Test response write failures are surfaced by the client assertions.
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.MergeConfig(nil, json.RawMessage(`{"start_urls":["`+srv.URL+`/docs/"],"path_prefix":"/docs/"}`))
	if err != nil {
		t.Fatal(err)
	}
	run := func(state json.RawMessage) ([]websource.RemotePage, *websource.FetchResult) {
		var pages []websource.RemotePage
		res, err := f.Fetch(context.Background(), &websource.Collection{Config: cfg}, nil, state, func(p websource.RemotePage) error {
			pages = append(pages, p)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return pages, res
	}

	pages, res := run(nil)
	titles := []string{}
	for _, p := range pages {
		if p.Err != nil {
			t.Fatalf("page error %v", p.Err)
		}
		titles = append(titles, p.Title)
	}
	if len(pages) != 3 || !res.Complete || hits["/docs/secret"] != 0 || hits["/blog/"] != 0 || hits["/docs/logo.png"] != 0 {
		t.Fatalf("titles=%v complete=%v hits=%v", titles, res.Complete, hits)
	}
	if !strings.Contains(strings.Join(titles, ","), "Guide") {
		t.Fatalf("titles = %v", titles)
	}

	pages, res = run(res.State)
	if len(pages) != 2 || res.Complete || len(res.Removed) != 0 {
		t.Fatalf("second run pages=%d complete=%v removed=%v", len(pages), res.Complete, res.Removed)
	}
}
