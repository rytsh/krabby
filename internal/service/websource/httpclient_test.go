package websource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Sources authenticate with headers net/http does not know are credentials.
// A redirect to another origin must not carry them; one on the same origin
// keeps them.
func TestDefaultClientStripsCredentialsOnCrossOriginRedirect(t *testing.T) {
	var gotToken, gotAccept string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotAccept = r.Header.Get("PRIVATE-TOKEN"), r.Header.Get("Accept")
	}))
	t.Cleanup(elsewhere.Close)

	var sameToken string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, elsewhere.URL+"/x", http.StatusFound)
		case "/here":
			http.Redirect(w, r, "/landed", http.StatusFound)
		default:
			sameToken = r.Header.Get("PRIVATE-TOKEN")
		}
	}))
	t.Cleanup(origin.Close)

	get := func(path string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("PRIVATE-TOKEN", "secret")
		req.Header.Set("Accept", "application/json")
		if _, err := Do(DefaultHTTPClient(), req, 1<<10); err != nil {
			t.Fatal(err)
		}
	}

	get("/away")
	if gotToken != "" {
		t.Errorf("PRIVATE-TOKEN %q followed a redirect to another origin", gotToken)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, a harmless header should survive the redirect", gotAccept)
	}

	get("/here")
	if sameToken != "secret" {
		t.Errorf("same-origin redirect lost PRIVATE-TOKEN: %q", sameToken)
	}
}
