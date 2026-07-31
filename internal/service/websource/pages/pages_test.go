package pages

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestFetchOneUsesRedirectURLAsRelativeBase(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/docs/start", http.StatusFound)
		case "/docs/start":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body><article><p>Read the <a href="next">next guide</a>.</p><img src="/assets/map.png" alt="Map"></article></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, markdown, err := New(nil).fetchOne(context.Background(), server.URL+"/old")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown, `[next guide](`+server.URL+`/docs/next)`) ||
		!strings.Contains(markdown, `![Map](`+server.URL+`/assets/map.png)`) {
		t.Fatalf("redirect-relative URLs were not resolved: %q", markdown)
	}
}

func TestFetchOneStripsCredentialsOnCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization") != "")
		_, _ = w.Write([]byte(`<html><body><main>Redirected documentation</main></body></html>`))
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("origin Authorization = %q", r.Header.Get("Authorization"))
		}
		http.Redirect(w, r, target.URL+"/page", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	if _, _, err := New(func(context.Context, string) (string, string, error) {
		return "", "secret", nil
	}).fetchOne(context.Background(), origin.URL+"/start"); err != nil {
		t.Fatal(err)
	}
	if leaked.Load() {
		t.Fatal("Authorization leaked to cross-origin redirect")
	}
}

func TestFetchOneRejectsUnsupportedAndOversizedBodies(t *testing.T) {
	t.Run("content type", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"message":"not documentation"}`))
		}))
		defer server.Close()

		_, _, err := New(nil).fetchOne(context.Background(), server.URL)
		if err == nil || !strings.Contains(err.Error(), "unsupported content type") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("body limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<article>" + strings.Repeat("x", maxBodyBytes) + "</article>"))
		}))
		defer server.Close()

		_, _, err := New(nil).fetchOne(context.Background(), server.URL)
		if err == nil || !strings.Contains(err.Error(), "page exceeds") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestFetchImageUsesOptionalCredentialsAndLimits(t *testing.T) {
	var imageBody bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&imageBody, img); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authenticated request header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageBody.Bytes())
	}))
	defer server.Close()

	fetcher := New(func(context.Context, string) (string, string, error) { return "", "secret", nil })
	content, err := fetcher.FetchImage(context.Background(), nil, server.URL+"/page", server.URL+"/image.png", 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	if !content.Authenticated || content.MediaType != "image/png" || len(content.Data) == 0 {
		t.Fatalf("content = %+v", content)
	}
	_, err = fetcher.FetchImage(context.Background(), nil, "https://docs.example/page", server.URL+"/image.png", 1<<20, false)
	if err == nil || !strings.Contains(err.Error(), "private-network") {
		t.Fatalf("private network error = %v", err)
	}
	_, err = fetcher.FetchImage(context.Background(), nil, server.URL+"/page", server.URL+"/image.png", 1, true)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("limit error = %v", err)
	}
}

func TestFetchImageRejectsAuthenticatedCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		http.Redirect(w, r, target.URL+"/stolen", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	fetcher := New(func(context.Context, string) (string, string, error) { return "", "secret", nil })
	_, err := fetcher.FetchImage(context.Background(), nil, origin.URL+"/page", origin.URL+"/image.png", 1<<20, true)
	if err == nil || !strings.Contains(err.Error(), "changed origin") {
		t.Fatalf("redirect error = %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("authenticated redirect reached target %d times", redirected.Load())
	}
}

func TestFetchSkipsManuallyAuthoredPages(t *testing.T) {
	fetcher := New(nil)
	var remotes []websource.RemotePage
	_, err := fetcher.Fetch(context.Background(), &websource.Collection{Name: "notes"}, []*websource.Page{
		{Slug: "manual-note", Title: "Manual note"},
	}, nil, func(p websource.RemotePage) error {
		remotes = append(remotes, p)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 0 {
		t.Fatalf("manual page was fetched: %+v", remotes)
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

func TestSitemapURLsRejectsCrossOriginChildren(t *testing.T) {
	t.Parallel()
	var childRequests atomic.Int32
	child := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		childRequests.Add(1)
	}))
	t.Cleanup(child.Close)
	root := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `<sitemapindex><sitemap><loc>%s/child.xml</loc></sitemap></sitemapindex>`, child.URL)
	}))
	t.Cleanup(root.Close)

	_, err := New(nil).SitemapURLs(context.Background(), root.URL+"/sitemap.xml")
	if err == nil || !strings.Contains(err.Error(), "changed origin") {
		t.Fatalf("cross-origin sitemap error = %v", err)
	}
	if childRequests.Load() != 0 {
		t.Fatalf("cross-origin child fetched %d times", childRequests.Load())
	}
}
