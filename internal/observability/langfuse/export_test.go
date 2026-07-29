package langfuse

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

// capture records what the exporter actually put on the wire.
type capture struct {
	mu     sync.Mutex
	got    bool
	path   string
	method string
	header http.Header
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.got = true
	c.path = r.URL.Path
	c.method = r.Method
	c.header = r.Header.Clone()
	c.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (c *capture) wait(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := c.got
		c.mu.Unlock()

		if got {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("exporter never sent a request")
}

// exportOnce runs a real export against a local server and returns what it saw.
func exportOnce(t *testing.T, host string) *capture {
	t.Helper()

	cap := &capture{}
	ts := httptest.NewServer(http.HandlerFunc(cap.handler))
	t.Cleanup(ts.Close)

	// The caller supplies the host shape to exercise (root or sub-path); the
	// test server's authority is substituted in.
	u, err := url.Parse(host)
	if err != nil {
		t.Fatalf("parse host template: %v", err)
	}

	tsURL, _ := url.Parse(ts.URL)
	u.Scheme = tsURL.Scheme
	u.Host = tsURL.Host

	tr, err := New(config.Langfuse{
		Enabled:   true,
		Host:      u.String(),
		PublicKey: "pk-lf-test",
		SecretKey: "sk-lf-test",
		TraceDocs: true,
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new tracer: %v", err)
	}

	_, end := tr.StartTrace(context.Background(), ScopeDocs, TraceInfo{Name: "probe"})
	end(nil, nil)

	// Shutdown flushes, which forces the batch out without waiting for the
	// batch timeout.
	if err := tr.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	cap.wait(t)

	return cap
}

// The exporter must POST to the signal-specific traces path.
//
// otlptracehttp.WithURLPath sets the path outright; it does not append
// "/v1/traces" the way OTEL_EXPORTER_OTLP_ENDPOINT does. Configuring Langfuse's
// base endpoint here posts to /api/public/otel, which answers 404 — a failure
// that only shows up as a dropped export, never as a startup error. This test
// exists to pin the full URL.
func TestExporterPostsToTracesEndpoint(t *testing.T) {
	got := exportOnce(t, "https://langfuse.example")

	if got.path != "/api/public/otel/v1/traces" {
		t.Fatalf("posted to %q, want /api/public/otel/v1/traces", got.path)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
}

// A Langfuse served under a sub-path must keep that prefix.
func TestExporterHonoursHostSubPath(t *testing.T) {
	got := exportOnce(t, "https://example.internal/langfuse")

	if got.path != "/langfuse/api/public/otel/v1/traces" {
		t.Fatalf("posted to %q, want /langfuse/api/public/otel/v1/traces", got.path)
	}
}

// A trailing slash on the configured host must not double up.
func TestExporterTrimsTrailingSlash(t *testing.T) {
	got := exportOnce(t, "https://example.internal/")

	if got.path != "/api/public/otel/v1/traces" {
		t.Fatalf("posted to %q, want /api/public/otel/v1/traces", got.path)
	}
}

// Authentication and the real-time ingestion header must travel with the batch.
func TestExporterSendsAuthAndIngestionHeaders(t *testing.T) {
	got := exportOnce(t, "https://langfuse.example")

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-lf-test:sk-lf-test"))
	if auth := got.header.Get("Authorization"); auth != want {
		t.Errorf("Authorization = %q, want %q", auth, want)
	}

	// Without this header Langfuse routes direct OTLP through the legacy path
	// and new data can take ten minutes to appear.
	if v := got.header.Get("X-Langfuse-Ingestion-Version"); v != "4" {
		t.Errorf("x-langfuse-ingestion-version = %q, want 4", v)
	}

	if ct := got.header.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("Content-Type = %q, want application/x-protobuf", ct)
	}
}
