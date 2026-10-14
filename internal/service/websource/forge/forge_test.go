package forge

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

func TestGitHubIssuesAndPulls(t *testing.T) {
	var srvURL string
	var gotSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v3/repos/acme/svc/issues":
			gotSince = r.URL.Query().Get("since")
			//nolint:errcheck // Test response write failures are surfaced by the client assertions.
			fmt.Fprintf(w, `[
{"number":1,"title":"Crash on start","body":"stack trace","state":"open","html_url":"https://gh/1","user":{"login":"ann"},
 "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","comments":1,"comments_url":"%s/api/v3/c/1","labels":[{"name":"bug"}]},
{"number":2,"title":"Fix crash","body":"closes #1","state":"closed","html_url":"https://gh/2","user":{"login":"bob"},
 "created_at":"2026-01-03T00:00:00Z","updated_at":"2026-01-04T00:00:00Z","comments":0,"labels":[],"pull_request":{"merged_at":"2026-01-04T00:00:00Z"}},
{"number":3,"title":"dup","state":"closed","user":{"login":"x"},"updated_at":"2026-01-05T00:00:00Z","labels":[{"name":"duplicate"}]}
]`, srvURL)
		case "/api/v3/c/1":
			_, _ = w.Write([]byte(`[{"user":{"login":"bob"},"body":"Null config on boot","created_at":"2026-01-02T00:00:00Z"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	f := New()
	cfg, err := f.MergeConfig(nil, json.RawMessage(`{"provider":"github","base_url":"`+srv.URL+`","repo":"acme/svc","token":"tok","exclude_labels":["duplicate"]}`))
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
	if len(pages) != 2 || !res.Complete {
		t.Fatalf("pages = %+v", pages)
	}
	if !strings.Contains(pages[0].Markdown, "Null config on boot") || !strings.Contains(pages[1].Markdown, "**State**: merged") ||
		pages[1].Slug != "pull-2" {
		t.Fatalf("markdown:\n%s\n---\n%s", pages[0].Markdown, pages[1].Markdown)
	}

	_, err = f.Fetch(context.Background(), &websource.Collection{Config: cfg}, nil, res.State, func(websource.RemotePage) error { return nil })
	if err != nil || !strings.HasPrefix(gotSince, "2026-01-04T23:59:59") {
		t.Fatalf("incremental since = %q, err %v", gotSince, err)
	}
}

func TestValidate(t *testing.T) {
	f := New()
	for _, bad := range []string{
		`{"provider":"svn","repo":"a/b"}`,
		`{"provider":"github","repo":"nope"}`,
		`{"provider":"gitea","repo":"a/b"}`,
		`{"provider":"github","repo":"a/b","issues":false,"pulls":false}`,
	} {
		if _, err := f.MergeConfig(nil, json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := f.MergeConfig(nil, json.RawMessage(`{"provider":"gitlab","repo":"group/sub/proj"}`)); err != nil {
		t.Fatal(err)
	}
}
