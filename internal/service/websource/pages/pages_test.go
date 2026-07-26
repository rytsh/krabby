package pages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestFetchCustomPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`<html><head><title>Custom Wiki</title></head><body>
			<article><h1>Fermentation</h1><p>Yeast converts sugar.</p></article>
		</body></html>`))
	}))
	defer server.Close()

	fetcher := New(func(context.Context, string) (string, string, error) {
		return "", "secret", nil
	})
	var remotes []websource.RemotePage
	res, err := fetcher.Fetch(context.Background(), &websource.Collection{Name: "wine"}, []*websource.Page{
		{Slug: "fermentation", URL: server.URL + "/fermentation"},
	}, nil, func(p websource.RemotePage) error {
		remotes = append(remotes, p)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A URL-list collection has no remote inventory to enumerate, so it must
	// never license the manager to delete registered pages.
	if res.Complete {
		t.Fatal("pages fetcher claimed a complete inventory")
	}

	if len(remotes) != 1 || remotes[0].Err != nil {
		t.Fatalf("remotes=%+v", remotes)
	}
	if !strings.Contains(remotes[0].Markdown, "Yeast converts sugar") {
		t.Fatalf("markdown=%q", remotes[0].Markdown)
	}
}
