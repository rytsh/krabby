package webdav

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

func TestWebDAVRecursiveSync(t *testing.T) {
	files := map[string]string{"/dav/Docs/a.md": "# A\n\nalpha", "/dav/Docs/sub/b.txt": "beta", "/dav/Docs/sub/x.bin": "?"}
	entry := func(href string, dir bool, etag string) string {
		rt := ""
		if dir {
			rt = "<d:collection/>"
		}
		return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>"%s"</d:getetag><d:resourcetype>%s</d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href, etag, rt)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "me" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == "PROPFIND" {
			w.WriteHeader(http.StatusMultiStatus)
			var b strings.Builder
			b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
			switch r.URL.Path {
			case "/dav/Docs/":
				b.WriteString(entry("/dav/Docs/", true, "") + entry("/dav/Docs/a.md", false, "e1") + entry("/dav/Docs/sub/", true, ""))
			case "/dav/Docs/sub/":
				b.WriteString(entry("/dav/Docs/sub/", true, "") + entry("/dav/Docs/sub/b.txt", false, "e2") + entry("/dav/Docs/sub/x.bin", false, "e3"))
			}
			b.WriteString(`</d:multistatus>`)
			_, _ = w.Write([]byte(b.String()))
			return
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.MergeConfig(nil, json.RawMessage(`{"url":"`+srv.URL+`/dav/Docs","user":"me","password":"pw"}`))
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
	for _, p := range pages {
		if p.Err != nil || !strings.HasPrefix(p.URL, srv.URL+"/dav/Docs/") {
			t.Fatalf("page = %+v", p)
		}
	}
}
