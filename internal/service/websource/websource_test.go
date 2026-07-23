package websource

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rakunlabs/bw"
)

func TestNamesAndScopeKeys(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{name: "wine", ok: true},
		{name: "confluence-x", ok: true},
		{name: "team.wiki_2", ok: true},
		{name: "Upper", ok: false},
		{name: "has space", ok: false},
		{name: "../escape", ok: false},
		{name: "", ok: false},
	}
	for _, tt := range tests {
		if got := ValidName(tt.name); got != tt.ok {
			t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.ok)
		}
	}

	if got := ScopeKey("wine"); got != "web:wine" {
		t.Fatalf("ScopeKey = %q", got)
	}
	if got := CollectionName("web:wine"); got != "wine" {
		t.Fatalf("CollectionName = %q", got)
	}
	if got := CollectionName("git.example.com/a/b"); got != "" {
		t.Fatalf("repo id recognized as collection: %q", got)
	}
}

func TestHTMLHelpers(t *testing.T) {
	md, err := MarkdownFromHTML(`<h1>Wine Guide</h1><p>Use <strong>Pinot</strong>.</p>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "# Wine Guide") || !strings.Contains(md, "**Pinot**") {
		t.Fatalf("unexpected markdown: %q", md)
	}

	title, article, err := ExtractArticle(`<!doctype html><html><head><title>Cellar</title></head><body>
		<nav>Menu noise</nav><article><h1>Wine Storage</h1><p>Keep it cool and dark.</p></article>
	</body></html>`, "https://wiki.example.com/cellar")
	if err != nil {
		t.Fatal(err)
	}
	if title == "" || !strings.Contains(article, "Keep it cool") {
		t.Fatalf("title=%q article=%q", title, article)
	}

	if got := Slugify("  Wine & Food / 2026  "); got != "wine-food-2026" {
		t.Fatalf("Slugify = %q", got)
	}
}

func TestStoreCollectionsAndPages(t *testing.T) {
	db, err := bw.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := New(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	col := &Collection{
		Name: "wine", Type: TypeConfluence, RefreshInterval: 24 * time.Hour,
		Config: json.RawMessage(`{"space":"WINE"}`),
	}
	if err := store.UpsertCollection(ctx, col); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPage(ctx, &Page{
		ID: PageID("wine", "42-guide"), Collection: "wine", Slug: "42-guide", URL: "https://wiki/p/42",
	}); err != nil {
		t.Fatal(err)
	}

	cols, err := store.ListCollections(ctx)
	if err != nil || len(cols) != 1 || cols[0].Name != "wine" {
		t.Fatalf("collections=%+v err=%v", cols, err)
	}
	if string(cols[0].Config) != `{"space":"WINE"}` {
		t.Fatalf("persisted config = %q", cols[0].Config)
	}
	pages, err := store.Pages(ctx, "wine")
	if err != nil || len(pages) != 1 || pages[0].Slug != "42-guide" {
		t.Fatalf("pages=%+v err=%v", pages, err)
	}

	if err := store.DeleteCollection(ctx, "wine"); err != nil {
		t.Fatal(err)
	}
	pages, err = store.Pages(ctx, "wine")
	if err != nil || len(pages) != 0 {
		t.Fatalf("pages after delete=%+v err=%v", pages, err)
	}
}

func TestFullResyncDue(t *testing.T) {
	// First run (zero time) always forces a full pass.
	if !FullResyncDue(time.Time{}, time.Hour) {
		t.Fatal("zero lastFull should be due")
	}
	// Recent full pass within the interval is not due.
	if FullResyncDue(time.Now().Add(-30*time.Minute), time.Hour) {
		t.Fatal("recent full pass should not be due")
	}
	// Older than the interval is due.
	if !FullResyncDue(time.Now().Add(-2*time.Hour), time.Hour) {
		t.Fatal("stale full pass should be due")
	}
	// Non-positive interval falls back to the default (24h).
	if FullResyncDue(time.Now().Add(-1*time.Hour), 0) {
		t.Fatal("with default interval a 1h-old pass should not be due")
	}
}
