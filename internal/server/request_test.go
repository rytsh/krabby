package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/observability/metrics"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/registry"
)

// limitQueue records the concurrency it was given.
type limitQueue struct {
	countingQueue
	limit int
	set   bool
}

func (q *limitQueue) SetTaskConcurrency(n int) { q.limit, q.set = n, true }

// A JSON body sent with another content type used to be skipped silently:
// the handler ran on a zero value and answered 200. It must be refused, as
// must a body that is not one JSON value or that exceeds the cap.
func TestJSONBodiesAreDecodedOrRefused(t *testing.T) {
	q := &limitQueue{}
	h := newRouter(&config.Config{MCP: config.MCP{Path: "/mcp"}},
		routeServices{queue: q, tracing: &manager.Manager{}}, MCPCatalogs{})

	put := func(contentType, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/concurrency", strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		return rec
	}

	for name, tc := range map[string]struct{ contentType, body string }{
		"form content type (curl -d)": {"application/x-www-form-urlencoded", `{"limit":4}`},
		"text/plain":                  {"text/plain", `{"limit":4}`},
		"empty body":                  {"application/json", ``},
		"two JSON values":             {"application/json", `{"limit":4}{"limit":5}`},
		"not JSON":                    {"application/json", `limit=4`},
		"over the size cap":           {"application/json", `{"limit":4,"pad":"` + strings.Repeat("x", defaultBodyLimit) + `"}`},
	} {
		q.set = false
		if rec := put(tc.contentType, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
		if q.set {
			t.Errorf("%s: the handler ran on a body it could not read", name)
		}
	}

	for name, ct := range map[string]string{"json": "application/json; charset=utf-8", "no content type": ""} {
		q.set = false
		if rec := put(ct, `{"limit":4}`); rec.Code >= 300 || !q.set || q.limit != 4 {
			t.Errorf("%s: status %d, limit set %v to %d; want it applied", name, rec.Code, q.set, q.limit)
		}
	}
}

// One status for every failure told a client nothing: an embedder outage
// read as a bad query, a disabled feature as a missing file.
func TestSendErrorMapsKinds(t *testing.T) {
	for want, err := range map[int]error{
		http.StatusNotFound:            fmt.Errorf("read; %w", os.ErrNotExist),
		http.StatusServiceUnavailable:  fmt.Errorf("search; %w", manager.ErrDocsDisabled),
		http.StatusConflict:            fmt.Errorf("resolve; %w", registry.ErrAmbiguousRepo),
		http.StatusGatewayTimeout:      fmt.Errorf("embed; %w", context.DeadlineExceeded),
		http.StatusBadRequest:          metrics.Invalid(errors.New("bad regex")),
		http.StatusInternalServerError: errors.New("index corrupt"),
	} {
		rec := httptest.NewRecorder()
		c := ada.NewContext(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		_ = sendError(c, err, http.StatusInternalServerError)
		if rec.Code != want {
			t.Errorf("sendError(%v) = %d, want %d", err, rec.Code, want)
		}
	}
}
