package s3

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

// fakeS3 serves ListObjectsV2 and GetObject for a path-style bucket.
func fakeS3(objects map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/bucket")
		key = strings.TrimPrefix(key, "/")
		if key == "" && r.URL.Query().Get("list-type") == "2" {
			var b strings.Builder
			b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>bucket</Name><IsTruncated>false</IsTruncated>`)
			for k, v := range objects {
				fmt.Fprintf(&b, `<Contents><Key>%s</Key><ETag>"%s"</ETag><Size>%d</Size><LastModified>2026-01-01T00:00:00.000Z</LastModified></Contents>`,
					k, websource.Hash(v), len(v))
			}
			b.WriteString(`</ListBucketResult>`)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(b.String()))
			return
		}
		body, ok := objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte(body))
	}))
}

func TestS3Sync(t *testing.T) {
	srv := fakeS3(map[string]string{"handbook/a.md": "# A\n\nalpha", "handbook/img.png": "x", "handbook/b.txt": "beta"})
	defer srv.Close()

	f := New()
	cfg, err := f.MergeConfig(nil, json.RawMessage(`{"bucket":"bucket","prefix":"handbook/","endpoint":"`+srv.URL+
		`","path_style":true,"access_key_id":"k","secret_access_key":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	view, _ := json.Marshal(f.ConfigView(cfg))
	if strings.Contains(string(view), `"s"`) {
		t.Fatalf("secret leaked: %s", view)
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
		if p.Err != nil {
			t.Fatalf("page error: %v", p.Err)
		}
	}
	pages = nil
	if _, err := f.Fetch(context.Background(), &websource.Collection{Config: cfg}, nil, res.State, func(p websource.RemotePage) error {
		pages = append(pages, p)
		return nil
	}); err != nil || len(pages) != 0 {
		t.Fatalf("unchanged rerun emitted %d pages (err %v)", len(pages), err)
	}
}
