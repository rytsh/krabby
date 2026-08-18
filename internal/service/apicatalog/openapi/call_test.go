package openapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// seenRequest captures what the API server actually received, which is the
// whole subject under test: Call's job is assembling a request, not parsing a
// response.
type seenRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

func callServer(t *testing.T, status int, respond string) (*httptest.Server, *seenRequest) {
	t.Helper()

	seen := &seenRequest{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*seen = seenRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   string(body),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respond))
	}))
	t.Cleanup(s.Close)

	return s, seen
}

func callDetail(method, path string) *apicatalog.Detail {
	return &apicatalog.Detail{
		Method: method,
		Path:   path,
		RequestBody: &apicatalog.Body{
			ContentType: "application/json",
		},
	}
}

func TestCallAssemblesRequest(t *testing.T) {
	server, seen := callServer(t, http.StatusOK, `{"ok":true}`)

	svc := &apicatalog.Service{
		Name:            "billing",
		ResolvedBaseURL: server.URL,
		Config:          json.RawMessage(fmt.Sprintf(`{"url":%q,"token":"stored-token"}`, server.URL)),
	}

	res, err := New().Call(context.Background(), svc, callDetail("POST", "/v1/invoices/{id}/send"), apicatalog.CallRequest{
		PathParams: map[string]string{"id": "inv_42"},
		Query:      map[string]string{"dry_run": "true"},
		Body:       json.RawMessage(`{"note":"hi"}`),
	})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}

	if !res.OK || res.Status != http.StatusOK {
		t.Fatalf("response = %+v, want 200 OK", res)
	}
	if seen.Method != "POST" || seen.Path != "/v1/invoices/inv_42/send" {
		t.Fatalf("server saw %s %s", seen.Method, seen.Path)
	}
	if seen.Query != "dry_run=true" {
		t.Fatalf("query = %q", seen.Query)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer stored-token" {
		t.Fatalf("Authorization = %q, want the stored token as bearer", got)
	}
	if got := seen.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if seen.Body != `{"note":"hi"}` {
		t.Fatalf("body = %q", seen.Body)
	}
	if res.Body != `{"ok":true}` {
		t.Fatalf("response body = %q", res.Body)
	}
	if res.URL == "" || !strings.Contains(res.URL, "/v1/invoices/inv_42/send") {
		t.Fatalf("resolved URL not reported: %q", res.URL)
	}
}

// TestCallHeaderPrecedence: a per-request header must beat the stored config,
// and an explicitly empty one must remove it — that pair is what makes "same
// endpoint, another identity" and "call it unauthenticated" possible.
func TestCallHeaderPrecedence(t *testing.T) {
	server, seen := callServer(t, http.StatusOK, `{}`)

	svc := &apicatalog.Service{
		Name:            "billing",
		ResolvedBaseURL: server.URL,
		Config: json.RawMessage(fmt.Sprintf(
			`{"url":%q,"token":"stored-token","headers":{"X-Env":"prod"}}`, server.URL)),
	}

	_, err := New().Call(context.Background(), svc, callDetail("GET", "/v1/me"), apicatalog.CallRequest{
		Headers: map[string]string{
			"Authorization": "Bearer caller-token",
			"X-Env":         "",
		},
	})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}

	if got := seen.Header.Get("Authorization"); got != "Bearer caller-token" {
		t.Fatalf("Authorization = %q, want the caller's token to win", got)
	}
	if _, ok := seen.Header["X-Env"]; ok {
		t.Fatalf("X-Env survived an explicit empty override")
	}
}

// TestCallErrorStatusIsAResult: a 500 from the peer is the answer the caller
// asked for, not a failure of the catalog.
func TestCallErrorStatusIsAResult(t *testing.T) {
	server, _ := callServer(t, http.StatusInternalServerError, `{"error":"boom"}`)

	svc := &apicatalog.Service{
		Name:            "billing",
		ResolvedBaseURL: server.URL,
		Config:          json.RawMessage(fmt.Sprintf(`{"url":%q}`, server.URL)),
	}

	res, err := New().Call(context.Background(), svc, callDetail("GET", "/v1/x"), apicatalog.CallRequest{})
	if err != nil {
		t.Fatalf("Call() error = %v, want the 500 as a result", err)
	}
	if res.OK || res.Status != http.StatusInternalServerError || res.Error != "" {
		t.Fatalf("response = %+v, want status 500, ok=false, no transport error", res)
	}
	if res.Body != `{"error":"boom"}` {
		t.Fatalf("body = %q", res.Body)
	}
}

// TestCallTransportFailure: a connection refused must come back through the
// response, so REST answers 200 with the diagnosis instead of a bare 500.
func TestCallTransportFailure(t *testing.T) {
	svc := &apicatalog.Service{
		Name:            "billing",
		ResolvedBaseURL: "http://127.0.0.1:1", // nothing listens on port 1
		Config:          json.RawMessage(`{"url":"http://127.0.0.1:1"}`),
	}

	res, err := New().Call(context.Background(), svc, callDetail("GET", "/v1/x"), apicatalog.CallRequest{})
	if err != nil {
		t.Fatalf("Call() error = %v, want the failure in the response", err)
	}
	if res.OK || res.Error == "" {
		t.Fatalf("response = %+v, want a transport error", res)
	}
}

func TestCallRefusesMissingPathParam(t *testing.T) {
	svc := &apicatalog.Service{
		Name:            "billing",
		ResolvedBaseURL: "https://api.example.com",
		Config:          json.RawMessage(`{"url":"https://api.example.com"}`),
	}

	_, err := New().Call(context.Background(), svc, callDetail("GET", "/v1/invoices/{id}"), apicatalog.CallRequest{})
	if err == nil || !strings.Contains(err.Error(), "missing path parameter") {
		t.Fatalf("error = %v, want a missing-parameter refusal", err)
	}
}

func TestCallServiceBaseURLOverrideWins(t *testing.T) {
	server, seen := callServer(t, http.StatusOK, `{}`)

	svc := &apicatalog.Service{
		Name: "billing",
		// The operator repointed the service; the detail still carries the old
		// host frozen at last sync.
		BaseURL:         server.URL,
		ResolvedBaseURL: "https://old.example.com",
		Config:          json.RawMessage(`{"url":"https://old.example.com"}`),
	}
	d := callDetail("GET", "/v1/me")
	d.BaseURL = "https://old.example.com"

	res, err := New().Call(context.Background(), svc, d, apicatalog.CallRequest{})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !res.OK || seen.Path != "/v1/me" {
		t.Fatalf("override did not repoint the call: %+v", res)
	}
}

// TestCallRedirectStripsCredentials: a cross-origin redirect is under the
// peer's control, and following it with the stored token is a credential leak.
func TestCallRedirectStripsCredentials(t *testing.T) {
	var elsewhereAuth string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(elsewhere.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/moved", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	svc := &apicatalog.Service{
		Name:            "billing",
		ResolvedBaseURL: origin.URL,
		Config:          json.RawMessage(fmt.Sprintf(`{"url":%q,"token":"secret"}`, origin.URL)),
	}

	res, err := New().Call(context.Background(), svc, callDetail("GET", "/v1/x"), apicatalog.CallRequest{})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !res.OK {
		t.Fatalf("redirected call failed: %+v", res)
	}
	if elsewhereAuth != "" {
		t.Fatalf("Authorization %q leaked across origins", elsewhereAuth)
	}
}
