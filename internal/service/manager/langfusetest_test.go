package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/settings"
)

// langfuseStub serves the two endpoints TestLangfuse consults.
func langfuseStub(t *testing.T, otlpStatus int) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/public/projects", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"name":"my-project"}]}`))
	})
	mux.HandleFunc(langfuse.TracesPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(otlpStatus)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return ts
}

// Valid keys alone are not enough: the OTLP endpoint has to exist. A Langfuse
// older than v3.22.0 serves the REST API but 404s OTLP, and the test used to
// report that as healthy while every export was discarded.
func TestTestLangfuseFailsWhenOTLPEndpointMissing(t *testing.T) {
	ts := langfuseStub(t, http.StatusNotFound)

	res := (&Manager{}).TestLangfuse(context.Background(), langfuseSettings(ts.URL))

	if res.OK {
		t.Fatal("reported ok while the OTLP endpoint returned 404")
	}

	if !strings.Contains(res.Error, "404") {
		t.Errorf("error does not mention the 404: %q", res.Error)
	}

	if !strings.Contains(res.Error, langfuse.TracesPath) {
		t.Errorf("error does not name the endpoint probed: %q", res.Error)
	}
}

func TestTestLangfuseSucceedsWhenBothEndpointsAnswer(t *testing.T) {
	ts := langfuseStub(t, http.StatusOK)

	res := (&Manager{}).TestLangfuse(context.Background(), langfuseSettings(ts.URL))

	if !res.OK {
		t.Fatalf("expected ok, got error %q", res.Error)
	}

	// The resolved project name is the thing that catches "right keys, wrong
	// project".
	if res.Model != "my-project" {
		t.Errorf("project = %q, want my-project", res.Model)
	}
}

// Credentials rejected by OTLP must surface, not be swallowed as a payload
// complaint.
func TestTestLangfuseFailsOnOTLPUnauthorized(t *testing.T) {
	ts := langfuseStub(t, http.StatusUnauthorized)

	res := (&Manager{}).TestLangfuse(context.Background(), langfuseSettings(ts.URL))

	if res.OK {
		t.Fatal("reported ok while OTLP rejected the keys")
	}
}

// A 4xx about the payload still proves the endpoint and credentials are good;
// the probe sends an empty body and some versions may object to it.
func TestTestLangfuseToleratesPayloadComplaint(t *testing.T) {
	ts := langfuseStub(t, http.StatusBadRequest)

	res := (&Manager{}).TestLangfuse(context.Background(), langfuseSettings(ts.URL))

	if !res.OK {
		t.Fatalf("a payload-level 400 must not fail the test: %q", res.Error)
	}
}

func TestProbeOTLPUsesTheExporterPath(t *testing.T) {
	var got string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if err := probeOTLP(context.Background(), ts.URL, config.Langfuse{PublicKey: "pk", SecretKey: "sk"}, 0); err != nil {
		t.Fatalf("probe: %v", err)
	}

	if got != langfuse.TracesPath {
		t.Fatalf("probed %q, want %q", got, langfuse.TracesPath)
	}
}

func langfuseSettings(host string) settings.Settings {
	return settings.Settings{
		LangfuseEnabled:   true,
		LangfuseHost:      host,
		LangfusePublicKey: "pk-lf-test",
		LangfuseSecretKey: "sk-lf-test",
	}
}

// Any server with a catch-all route answers 200 on the projects path - krabby's
// own SPA fallback does. The test must check the shape of the reply, not just
// its status, or it would pass against any web server at that address.
func TestTestLangfuseRejectsNonLangfuseHost(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><html><body>some app</body></html>"))
	}))
	defer ts.Close()

	res := (&Manager{}).TestLangfuse(context.Background(), langfuseSettings(ts.URL))

	if res.OK {
		t.Fatal("reported ok against a host that is not Langfuse")
	}

	if !strings.Contains(res.Error, "project list") {
		t.Errorf("error is not explanatory: %q", res.Error)
	}
}
