package manager

import (
	"context"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

type sitemapTestFetcher struct {
	fakeReconcileFetcher
	urls []string
}

func (f *sitemapTestFetcher) SitemapURLs(context.Context, string) ([]string, error) {
	return f.urls, nil
}

func TestImportWebSitemapAddsNewPagesOnce(t *testing.T) {
	ctx := context.Background()
	fetcher := &sitemapTestFetcher{urls: []string{
		"https://example.com/alpha",
		"https://example.com/beta",
	}}
	m, store := newReconcileManager(t, fetcher)
	m.webFetchers[websource.TypePages] = fetcher

	if err := store.UpsertCollection(ctx, &websource.Collection{
		Name: "site", Type: websource.TypePages, Status: websource.StatusPending,
	}); err != nil {
		t.Fatal(err)
	}

	release := blockQueue(t, m.queue)
	defer release()

	result, err := m.ImportWebSitemap(ctx, "site", "https://example.com/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}
	if result.Discovered != 2 || result.Added != 2 || result.Existing != 0 {
		t.Fatalf("first import = %+v", result)
	}

	result, err = m.ImportWebSitemap(ctx, "site", "https://example.com/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}
	if result.Discovered != 2 || result.Added != 0 || result.Existing != 2 {
		t.Fatalf("second import = %+v", result)
	}

	pages, err := store.Pages(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}
}
