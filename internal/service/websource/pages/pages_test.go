package pages

import (
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestFetchCustomPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`<html><head><title>Custom Wiki</title></head><body>
			<article><h1>Fermentation</h1><p>Yeast converts sugar.</p></article>
		</body></html>`))
	}))
	defer server.Close()

	fetcher := New(func(context.Context, string) (string, string, error) {
		return "", "secret", nil
	})
	var remotes []websource.RemotePage
	res, err := fetcher.Fetch(context.Background(), &websource.Collection{Name: "wine"}, []*websource.Page{
		{Slug: "fermentation", URL: server.URL + "/fermentation"},
	}, nil, func(p websource.RemotePage) error {
		remotes = append(remotes, p)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A URL-list collection has no remote inventory to enumerate, so it must
	// never license the manager to delete registered pages.
	if res.Complete {
		t.Fatal("pages fetcher claimed a complete inventory")
	}

	if len(remotes) != 1 || remotes[0].Err != nil {
		t.Fatalf("remotes=%+v", remotes)
	}
	if !strings.Contains(remotes[0].Markdown, "Yeast converts sugar") {
		t.Fatalf("markdown=%q", remotes[0].Markdown)
	}
}

func TestSitemapURLsFollowsIndexesAndDeduplicates(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}

		switch r.URL.Path {
		case "/sitemap.xml":
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
				<sitemap><loc>%s/first.xml</loc></sitemap>
				<sitemap><loc>/second.xml.gz</loc></sitemap>
			</sitemapindex>`, server.URL)
		case "/first.xml":
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
				<url><loc>%s/alpha</loc></url>
				<url><loc>%s/shared</loc></url>
				<url><loc>ftp://example.com/ignored</loc></url>
			</urlset>`, server.URL, server.URL)
		case "/second.xml.gz":
			w.Header().Set("Content-Type", "application/gzip")
			zw := gzip.NewWriter(w)
			_, _ = fmt.Fprintf(zw, `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
				<url><loc>%s/shared</loc></url>
				<url><loc>%s/beta</loc></url>
			</urlset>`, server.URL, server.URL)
			_ = zw.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fetcher := New(func(context.Context, string) (string, string, error) {
		return "", "secret", nil
	})
	urls, err := fetcher.SitemapURLs(context.Background(), server.URL+"/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{server.URL + "/alpha", server.URL + "/shared", server.URL + "/beta"}
	if len(urls) != len(want) {
		t.Fatalf("urls = %v, want %v", urls, want)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Fatalf("urls = %v, want %v", urls, want)
		}
	}
}

func TestSitemapURLsRejectsNonSitemapXML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>not a sitemap</body></html>`))
	}))
	defer server.Close()

	_, err := New(nil).SitemapURLs(context.Background(), server.URL)
	if err == nil || !strings.Contains(err.Error(), `unsupported sitemap root "html"`) {
		t.Fatalf("error = %v", err)
	}
}
