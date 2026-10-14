package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestNotionDatabaseSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Notion-Version") == "" || r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/databases/db1/query":
			_, _ = w.Write([]byte(`{"results":[{"object":"page","id":"p1","url":"https://notion.so/p1","last_edited_time":"2026-10-01T10:00:00.000Z",
"properties":{"Name":{"type":"title","title":[{"plain_text":"Incident playbook"}]},"Owner":{"type":"select","select":{"name":"SRE"}}}}],"has_more":false}`))
		case "/blocks/p1/children":
			_, _ = w.Write([]byte(`{"results":[
{"id":"b1","type":"heading_1","heading_1":{"rich_text":[{"plain_text":"Steps"}]}},
{"id":"b2","type":"numbered_list_item","has_children":true,"numbered_list_item":{"rich_text":[{"plain_text":"Page "},{"plain_text":"on-call","annotations":{"bold":true}}]}},
{"id":"b4","type":"code","code":{"language":"bash","rich_text":[{"plain_text":"kubectl rollout undo"}]}}],"has_more":false}`))
		case "/blocks/b2/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"b3","type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"use PagerDuty"}]}}],"has_more":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	f := New()
	f.api = srv.URL
	raw, err := f.MergeConfig(nil, json.RawMessage(`{"token":"secret","database_id":"db1"}`))
	if err != nil {
		t.Fatal(err)
	}
	var pages []websource.RemotePage
	res, err := f.Fetch(context.Background(), &websource.Collection{Config: raw}, nil, nil, func(p websource.RemotePage) error {
		pages = append(pages, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || !res.Complete {
		t.Fatalf("pages = %+v", pages)
	}
	md := pages[0].Markdown
	for _, want := range []string{"**Owner**: SRE", "## Steps", "1. Page **on-call**", "  - use PagerDuty", "```bash\nkubectl rollout undo"} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in:\n%s", want, md)
		}
	}
	if pages[0].Title != "Incident playbook" || pages[0].URL != "https://notion.so/p1" {
		t.Fatalf("page = %+v", pages[0])
	}
}

func TestCleanID(t *testing.T) {
	if got := cleanID("https://www.notion.so/team/Runbooks-0123456789abcdef0123456789abcdef?v=1"); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("cleanID = %q", got)
	}
}
