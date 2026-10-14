package gdrive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

const fakeKey = `{"type":"service_account","client_email":"bot@p.iam.gserviceaccount.com"}`

func TestDriveTreeExportAndDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/files" && strings.Contains(r.URL.Query().Get("q"), "'root1' in parents"):
			_, _ = w.Write([]byte(`{"files":[
{"id":"d1","name":"Design","mimeType":"application/vnd.google-apps.document","version":"7","webViewLink":"https://docs.google.com/d1"},
{"id":"f1","name":"Specs","mimeType":"application/vnd.google-apps.folder"},
{"id":"x1","name":"Form","mimeType":"application/vnd.google-apps.form"}]}`))
		case r.URL.Path == "/files" && strings.Contains(r.URL.Query().Get("q"), "'f1' in parents"):
			_, _ = w.Write([]byte(`{"files":[{"id":"m1","name":"api.md","mimeType":"text/markdown","md5Checksum":"abc","size":"20"}]}`))
		case r.URL.Path == "/files/d1/export":
			if r.URL.Query().Get("mimeType") != "text/html" {
				t.Errorf("export mime = %q", r.URL.Query().Get("mimeType"))
			}
			_, _ = w.Write([]byte(`<html><body><h1>Design</h1><p>event sourcing</p></body></html>`))
		case r.URL.Path == "/files/m1":
			_, _ = w.Write([]byte("# API\n\nREST v2"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	f := New()
	f.api = srv.URL
	f.client = func(context.Context, Config) (*http.Client, error) { return srv.Client(), nil }
	key, _ := json.Marshal(fakeKey)
	raw, err := f.MergeConfig(nil, json.RawMessage(`{"service_account_json":`+string(key)+`,"folder_id":"root1"}`))
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
	if len(pages) != 2 || !res.Complete {
		t.Fatalf("pages = %+v", pages)
	}
	joined := pages[0].Markdown + pages[1].Markdown
	for _, want := range []string{"event sourcing", "REST v2", "Specs/api.md"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q:\n%s", want, joined)
		}
	}
	view, _ := json.Marshal(f.ConfigView(raw))
	if strings.Contains(string(view), "client_email") {
		t.Fatalf("key leaked: %s", view)
	}
}
