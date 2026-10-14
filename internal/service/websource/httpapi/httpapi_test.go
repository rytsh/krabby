package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

func collect(t *testing.T, f *Fetcher, cfg map[string]any, state json.RawMessage) ([]websource.RemotePage, *websource.FetchResult) {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	merged, err := f.MergeConfig(nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	var pages []websource.RemotePage
	res, err := f.Fetch(context.Background(), &websource.Collection{Name: "t", Config: merged}, nil, state,
		func(p websource.RemotePage) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatal(err)
	}

	return pages, res
}

func TestOffsetPaginationAndMapping(t *testing.T) {
	var gotAuth, gotSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Api-Key")
		gotSince = r.URL.Query().Get("updated_since")
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		var items []map[string]any
		for i := off; i < min(off+2, 3); i++ {
			items = append(items, map[string]any{
				"id": i, "subject": "Ticket " + strconv.Itoa(i), "body": "<p>disk <b>full</b></p>",
				"updated_at": "2026-01-0" + strconv.Itoa(i+1) + "T10:00:00Z", "group": map[string]any{"name": "SRE"},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items}})
	}))
	defer srv.Close()

	f := New()
	cfg := map[string]any{
		"url": srv.URL, "items_path": "data.items", "pagination": "offset", "page_size": 2,
		"id_path": "id", "title_path": "subject", "body_path": "body", "body_format": "html",
		"updated_path": "updated_at", "team_paths": []string{"group.name"},
		"url_template": "https://tracker.example.com/t/{{.id}}", "since_param": "updated_since",
		"headers": map[string]string{"X-Api-Key": "k"},
	}
	pages, res := collect(t, f, cfg, nil)
	if len(pages) != 3 || !res.Complete {
		t.Fatalf("pages=%d complete=%v", len(pages), res.Complete)
	}
	p := pages[1]
	if p.Title != "Ticket 1" || p.URL != "https://tracker.example.com/t/1" || !strings.Contains(p.Markdown, "disk **full**") ||
		len(p.Teams) != 1 || p.Teams[0] != "SRE" || p.UpdatedAt.Day() != 2 {
		t.Fatalf("page = %+v", p)
	}
	if gotAuth != "k" || gotSince != "" {
		t.Fatalf("auth=%q since=%q", gotAuth, gotSince)
	}

	_, res = collect(t, f, cfg, res.State)
	if res.Complete || !strings.HasPrefix(gotSince, "2026-01-03") {
		t.Fatalf("incremental complete=%v since=%q", res.Complete, gotSince)
	}
}

func TestCursorAndTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"items":[{"key":"A"}],"next":"c2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"key":"B"}],"next":""}`))
	}))
	defer srv.Close()

	pages, _ := collect(t, New(), map[string]any{
		"url": srv.URL, "items_path": "items", "pagination": "cursor", "cursor_path": "next",
		"id_path": "key", "template": "Item {{.key}}",
	}, nil)
	if len(pages) != 2 || pages[1].Markdown != "Item B" {
		t.Fatalf("pages = %+v", pages)
	}
}

func TestValidateAndRedact(t *testing.T) {
	f := New()
	if _, err := f.MergeConfig(nil, json.RawMessage(`{"url":"ftp://x","id_path":"id"}`)); err == nil {
		t.Fatal("ftp accepted")
	}
	if _, err := f.MergeConfig(nil, json.RawMessage(`{"url":"https://x","id_path":"id","bogus":1}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	cur, err := f.MergeConfig(nil, json.RawMessage(`{"url":"https://x","id_path":"id","token":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.MergeConfig(cur, json.RawMessage(`{"token":"","title_path":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := decode(next)
	if cfg.Token != "s" || cfg.TitlePath != "t" {
		t.Fatalf("merged = %+v", cfg)
	}
	view, _ := json.Marshal(f.ConfigView(next))
	if strings.Contains(string(view), `"s"`) || !strings.Contains(string(view), `"token_set":true`) {
		t.Fatalf("view leaks: %s", view)
	}
}
