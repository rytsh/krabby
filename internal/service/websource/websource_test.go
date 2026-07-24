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

func TestPagesPagedAndTeamFilter(t *testing.T) {
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
	if err := store.UpsertCollection(ctx, &Collection{Name: "proj", Type: TypeJira}); err != nil {
		t.Fatal(err)
	}

	// Six pages; some tagged with teams (mixed casing), some untagged.
	specs := []struct {
		slug  string
		teams []string
	}{
		{"01", []string{"Alpha"}},
		{"02", []string{"beta"}},
		{"03", []string{"ALPHA", "Gamma"}},
		{"04", nil},
		{"05", []string{"Beta"}},
		{"06", []string{"gamma"}},
	}
	for _, s := range specs {
		if err := store.UpsertPage(ctx, &Page{
			ID: PageID("proj", s.slug), Collection: "proj", Slug: s.slug,
			URL: "https://x/" + s.slug, Teams: s.teams,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Unfiltered count is every page.
	if n, err := store.CountPages(ctx, "proj", ""); err != nil || n != 6 {
		t.Fatalf("CountPages all = %d, %v; want 6", n, err)
	}

	// Team filter is case-insensitive and matches "has any".
	if n, err := store.CountPages(ctx, "proj", "alpha"); err != nil || n != 2 {
		t.Fatalf("CountPages alpha = %d, %v; want 2", n, err)
	}
	if n, err := store.CountPages(ctx, "proj", "BETA"); err != nil || n != 2 {
		t.Fatalf("CountPages BETA = %d, %v; want 2", n, err)
	}
	if n, err := store.CountPages(ctx, "proj", "gamma"); err != nil || n != 2 {
		t.Fatalf("CountPages gamma = %d, %v; want 2", n, err)
	}
	if n, err := store.CountPages(ctx, "proj", "missing"); err != nil || n != 0 {
		t.Fatalf("CountPages missing = %d, %v; want 0", n, err)
	}

	// Pagination returns a bounded, slug-sorted window with the full total.
	first, total, err := store.PagesPaged(ctx, "proj", "", 0, 2)
	if err != nil || total != 6 || len(first) != 2 {
		t.Fatalf("page1 = %d items total %d, %v; want 2 items total 6", len(first), total, err)
	}
	if first[0].Slug != "01" || first[1].Slug != "02" {
		t.Fatalf("page1 slugs = %q,%q; want 01,02", first[0].Slug, first[1].Slug)
	}

	second, _, err := store.PagesPaged(ctx, "proj", "", 2, 2)
	if err != nil || len(second) != 2 || second[0].Slug != "03" {
		t.Fatalf("page2 = %+v, %v; want start 03", second, err)
	}

	// Team-filtered pagination scopes both the total and the window.
	alpha, aTotal, err := store.PagesPaged(ctx, "proj", "Alpha", 0, 10)
	if err != nil || aTotal != 2 || len(alpha) != 2 {
		t.Fatalf("alpha page = %d items total %d, %v; want 2/2", len(alpha), aTotal, err)
	}
	for _, p := range alpha {
		if p.Slug != "01" && p.Slug != "03" {
			t.Fatalf("unexpected alpha page slug %q", p.Slug)
		}
	}

	// Distinct teams preserve first-seen casing (by slug order) and are sorted
	// case-insensitively. "beta" is first seen on page 02 in lowercase; "Alpha"
	// on page 01; "Gamma" on page 03.
	teams, err := store.Teams(ctx, "proj")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Alpha", "beta", "Gamma"}
	if len(teams) != len(want) {
		t.Fatalf("teams = %v; want %v", teams, want)
	}
	for i := range want {
		if teams[i] != want[i] {
			t.Fatalf("teams = %v; want %v", teams, want)
		}
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
