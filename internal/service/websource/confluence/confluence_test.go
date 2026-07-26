package confluence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestLabelSelected(t *testing.T) {
	var page contentPage
	page.Metadata.Labels.Results = append(page.Metadata.Labels.Results,
		struct {
			Name string `json:"name"`
		}{Name: "Published"},
		struct {
			Name string `json:"name"`
		}{Name: "Wine"},
	)

	tests := []struct {
		name             string
		include, exclude []string
		want             bool
	}{
		{name: "no filters", want: true},
		{name: "included", include: []string{"wine"}, want: true},
		{name: "include case insensitive", include: []string{"PUBLISHED"}, want: true},
		{name: "missing include", include: []string{"beer"}, want: false},
		{name: "excluded", exclude: []string{"wine"}, want: false},
		{name: "exclude wins", include: []string{"published"}, exclude: []string{"WINE"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := labelSelected(page, tt.include, tt.exclude); got != tt.want {
				t.Fatalf("labelSelected() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigMergeAndRedaction(t *testing.T) {
	f := New()
	current := json.RawMessage(`{"base_url":"https://wiki.example.com","space":"OLD","user":"a@example.com","api_token":"secret"}`)
	update := json.RawMessage(`{"base_url":"https://wiki.example.com/","space":"WINE","user":"a@example.com","api_token":"","include_labels":["published"]}`)

	merged, err := f.MergeConfig(current, update)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeConfig(merged)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIToken != "secret" || cfg.Space != "WINE" || cfg.BaseURL != "https://wiki.example.com" {
		t.Fatalf("merged config = %#v", cfg)
	}

	view, ok := f.ConfigView(merged).(configView)
	if !ok || !view.APITokenSet {
		t.Fatalf("redacted view = %#v", view)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || strings.Contains(string(raw), "secret") {
		t.Fatalf("secret leaked in view: %s", raw)
	}
}

func TestValidateSpaceOrRootPage(t *testing.T) {
	f := New()
	tests := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "space", raw: `{"base_url":"https://w.example.com","space":"FIN"}`, ok: true},
		{name: "root page", raw: `{"base_url":"https://w.example.com","root_page":"1254228318"}`, ok: true},
		{name: "neither", raw: `{"base_url":"https://w.example.com"}`, ok: false},
		{name: "no base", raw: `{"root_page":"123"}`, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := f.Validate(json.RawMessage(tt.raw)); (err == nil) != tt.ok {
				t.Fatalf("Validate() err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestFirstEndpoint(t *testing.T) {
	// Subtree mode: CQL descendant query.
	sub := firstEndpoint(resolvedConfig{RootPage: "1254228318"}, "")
	if !strings.Contains(sub, "/rest/api/content/search") ||
		!strings.Contains(sub, "ancestor") {
		t.Fatalf("subtree endpoint = %q", sub)
	}

	// Subtree scoped to a space adds a space clause.
	subSpace := firstEndpoint(resolvedConfig{RootPage: "1", Space: "FIN"}, "")
	if !strings.Contains(subSpace, "space") {
		t.Fatalf("scoped subtree endpoint = %q", subSpace)
	}

	// Space mode: CQL space query (uniform CQL for incremental support).
	space := firstEndpoint(resolvedConfig{Space: "FIN"}, "")
	if !strings.Contains(space, "/rest/api/content/search") || !strings.Contains(space, "space") {
		t.Fatalf("space endpoint = %q", space)
	}
	if !strings.Contains(space, "lastmodified+ASC") && !strings.Contains(space, "lastmodified%20ASC") {
		t.Fatalf("expected ascending order for monotonic watermark: %q", space)
	}

	// Incremental: watermark adds a lastmodified clause.
	inc := firstEndpoint(resolvedConfig{RootPage: "1"}, "2025-01-02 15:04")
	if !strings.Contains(inc, "lastmodified") {
		t.Fatalf("incremental endpoint missing lastmodified: %q", inc)
	}
}

func TestMergeConfig(t *testing.T) {
	f := New()
	stored := json.RawMessage(`{"base_url":"https://c.example.com","root_page":"123","include_root":true,"api_token":"SECRET","space":"FIN"}`)

	decode := func(t *testing.T, raw json.RawMessage) resolvedConfig {
		t.Helper()
		cfg, err := decodeConfig(raw)
		if err != nil {
			t.Fatalf("decode merged config: %v", err)
		}

		return cfg
	}

	// Description-only update (empty config object): every stored field is kept,
	// including the write-only token.
	t.Run("empty update keeps everything", func(t *testing.T) {
		merged, err := f.MergeConfig(stored, json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, merged)
		if got.BaseURL != "https://c.example.com" || got.RootPage != "123" ||
			got.Space != "FIN" || got.APIToken != "SECRET" || !got.IncludeRoot {
			t.Fatalf("empty update lost fields: %+v", got)
		}
	})

	// A value overrides only that field; others (and the token) are kept.
	t.Run("value overrides one field", func(t *testing.T) {
		merged, err := f.MergeConfig(stored, json.RawMessage(`{"root_page":"999"}`))
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, merged)
		if got.RootPage != "999" {
			t.Fatalf("root_page not overridden: %q", got.RootPage)
		}
		if got.BaseURL != "https://c.example.com" || got.APIToken != "SECRET" {
			t.Fatalf("override wiped other fields: %+v", got)
		}
	})

	// A blank api_token keeps the stored secret (tokens are write-only).
	t.Run("blank token keeps secret", func(t *testing.T) {
		merged, err := f.MergeConfig(stored, json.RawMessage(`{"api_token":""}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := decode(t, merged); got.APIToken != "SECRET" {
			t.Fatalf("blank token should keep secret, got %q", got.APIToken)
		}
	})

	// A new api_token replaces the stored one.
	t.Run("new token replaces", func(t *testing.T) {
		merged, err := f.MergeConfig(stored, json.RawMessage(`{"api_token":"NEW"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := decode(t, merged); got.APIToken != "NEW" {
			t.Fatalf("token not replaced, got %q", got.APIToken)
		}
	})

	// An explicit null clears an optional field (space), while a space or root
	// page must remain for the config to stay valid.
	t.Run("explicit null clears field", func(t *testing.T) {
		merged, err := f.MergeConfig(stored, json.RawMessage(`{"space":null}`))
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, merged)
		if got.Space != "" {
			t.Fatalf("space should be cleared, got %q", got.Space)
		}
		if got.RootPage != "123" {
			t.Fatalf("root_page should remain: %q", got.RootPage)
		}
	})
}

func TestBreadcrumb(t *testing.T) {
	// No ancestors: empty breadcrumb.
	if got := (contentPage{}).breadcrumb(); got != "" {
		t.Fatalf("no ancestors = %q, want empty", got)
	}

	// Ancestor titles joined root->parent order, blanks skipped.
	var p contentPage
	p.Ancestors = []struct {
		Title string `json:"title"`
	}{
		{Title: "Financial Operations"},
		{Title: " "},
		{Title: "Landscapes"},
		{Title: "SDD Brazil Invoicing"},
	}
	want := "Financial Operations › Landscapes › SDD Brazil Invoicing"
	if got := p.breadcrumb(); got != want {
		t.Fatalf("breadcrumb = %q, want %q", got, want)
	}
}

func TestParseConfluenceTime(t *testing.T) {
	if parseConfluenceTime("2026-07-23T12:44:38.000Z").IsZero() {
		t.Fatal("failed to parse RFC3339 version.when")
	}
	if parseConfluenceTime("2025-01-02 15:04").IsZero() {
		t.Fatal("failed to parse watermark format")
	}
	if !parseConfluenceTime("nonsense").IsZero() {
		t.Fatal("garbage should be zero time")
	}
}

// TestSlugIsStableAcrossRenames pins the identity contract. The slug used to
// carry the page title, so renaming a page changed its identity: the
// incremental run (which sees the bumped last-modified date) emitted it as a
// new page, and the previous record, markdown and vectors stayed behind because
// pruning requires a complete sweep. Search then returned the document twice.
func TestSlugIsStableAcrossRenames(t *testing.T) {
	page := contentPage{ID: "123456", Title: "Deployment Guide"}
	renamed := contentPage{ID: "123456", Title: "Deployment Guide (2026)"}

	before := pageToRemote("https://x", page)
	after := pageToRemote("https://x", renamed)

	if before.Slug != after.Slug {
		t.Fatalf("renaming changed the slug: %q -> %q", before.Slug, after.Slug)
	}

	if before.Slug != "123456" {
		t.Fatalf("slug = %q, want the bare page id", before.Slug)
	}

	// The title is not lost; it travels on the record instead.
	if after.Title != "Deployment Guide (2026)" {
		t.Fatalf("title = %q", after.Title)
	}
}

// TestSlugGenerationForcesFullSweep covers the migration: collections synced
// under the old slug format hold records keyed by it, and only a complete sweep
// can re-emit them under the new one and prune the leftovers in the same run.
func TestSlugGenerationForcesFullSweep(t *testing.T) {
	old, err := json.Marshal(syncState{Watermark: "2026-01-01 00:00", FullAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}

	var state syncState
	if err := json.Unmarshal(old, &state); err != nil {
		t.Fatal(err)
	}

	// The stored state is recent enough that the periodic full pass is not due,
	// so the generation check is the only thing that can force one.
	if websource.FullResyncDue(state.FullAt, 24*time.Hour) {
		t.Fatal("precondition: the periodic full pass must not be due")
	}
	if state.V >= slugGeneration {
		t.Fatalf("state from the old format reports generation %d", state.V)
	}

	current, err := json.Marshal(syncState{Watermark: "x", FullAt: time.Now(), V: slugGeneration})
	if err != nil {
		t.Fatal(err)
	}
	var fresh syncState
	if err := json.Unmarshal(current, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.V < slugGeneration {
		t.Fatal("a state written now must not ask for another migration sweep")
	}
}
