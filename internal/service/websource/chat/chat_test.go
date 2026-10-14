package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestSlackThreads(t *testing.T) {
	now := time.Now().Unix()
	ts := func(off int64) string { return strconv.FormatInt(now-off, 10) + ".000100" }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"not_authed"}`))
			return
		}
		switch r.URL.Path {
		case "/conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[
{"ts":"` + ts(100) + `","user":"U1","text":"How do I rotate the DB password?","reply_count":1,"thread_ts":"` + ts(100) + `"},
{"ts":"` + ts(90) + `","bot_id":"B1","text":"deploy finished"},
{"ts":"` + ts(80) + `","user":"U2","text":"my token is abc"}]}`))
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[
{"ts":"` + ts(100) + `","user":"U1","text":"root"},
{"ts":"` + ts(50) + `","user":"U2","text":"Use vault rotate db/creds","thread_ts":"` + ts(100) + `"}]}`))
		case "/users.info":
			name := map[string]string{"U1": "ann", "U2": "bob"}[r.URL.Query().Get("user")]
			_, _ = w.Write([]byte(`{"ok":true,"user":{"name":"` + name + `"}}`))
		case "/conversations.info":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"name":"ops"}}`))
		}
	}))
	defer srv.Close()

	f := New()
	raw, err := f.MergeConfig(nil, json.RawMessage(`{"provider":"slack","token":"xoxb","channels":["C1"],"exclude_pattern":"(?i)token"}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := decode(raw)
	ad := &slack{client: f.client, cfg: cfg, users: map[string]string{}, base: srv.URL}
	name, msgs, err := ad.messages(context.Background(), "C1", time.Now().AddDate(0, 0, -1))
	if err != nil || name != "ops" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	threads := group(cfg, "C1", msgs)
	if len(threads) != 1 || len(threads[0].Replies) != 1 {
		t.Fatalf("threads = %+v", threads)
	}
	page := render(cfg, name, threads[0])
	if !strings.Contains(page.Markdown, "**bob**") || !strings.Contains(page.Markdown, "vault rotate") || !websource.ValidSlug(page.Slug) {
		t.Fatalf("page = %+v", page)
	}

	cfg.RedactUsers = true
	if strings.Contains(render(cfg, name, threads[0]).Markdown, "bob") {
		t.Fatal("user name not redacted")
	}
}

func TestValidate(t *testing.T) {
	f := New()
	for _, bad := range []string{
		`{"provider":"irc","channels":["x"]}`,
		`{"provider":"slack","channels":[]}`,
		`{"provider":"mattermost","channels":["x"]}`,
		`{"provider":"teams","channels":["x"]}`,
		`{"provider":"slack","channels":["x"],"exclude_pattern":"("}`,
	} {
		if _, err := f.MergeConfig(nil, json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
