package sharepoint

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestSharePointLibrary(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sites/contoso.sharepoint.com:/sites/Eng":
			_, _ = w.Write([]byte(`{"id":"site1"}`))
		case "/sites/site1/drive/root:/Runbooks:/children":
			_, _ = w.Write([]byte(`{"value":[
{"id":"i1","name":"failover.md","cTag":"c1","size":10,"webUrl":"https://contoso.sharepoint.com/f","file":{"mimeType":"text/markdown"}},
{"id":"i2","name":"Old","folder":{}}]}`))
		case "/sites/site1/drive/items/i2/children":
			_, _ = w.Write([]byte(`{"value":[{"id":"i3","name":"notes.txt","cTag":"c3","size":5,"file":{"mimeType":"text/plain"}}],"@odata.nextLink":""}`))
		case "/sites/site1/drive/items/i1/content":
			_, _ = w.Write([]byte("# Failover\n\nswitch region"))
		case "/sites/site1/drive/items/i3/content":
			_, _ = w.Write([]byte("legacy"))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	f := New()
	f.graph = srvURL
	f.client = func(context.Context, Config) *http.Client { return srv.Client() }
	raw, err := f.MergeConfig(nil, json.RawMessage(`{"tenant_id":"t","client_id":"c","client_secret":"s","site_url":"https://contoso.sharepoint.com/sites/Eng","folder_path":"/Runbooks/","exclude":["Old/**"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var pages []websource.RemotePage
	if _, err := f.Fetch(context.Background(), &websource.Collection{Config: raw}, nil, nil, func(p websource.RemotePage) error {
		pages = append(pages, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || !strings.Contains(pages[0].Markdown, "switch region") || pages[0].URL != "https://contoso.sharepoint.com/f" {
		t.Fatalf("pages = %+v", pages)
	}
	if _, err := f.MergeConfig(nil, json.RawMessage(`{"tenant_id":"t","client_id":"c","client_secret":"s"}`)); err == nil {
		t.Fatal("missing site accepted")
	}
}
