package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The guard sits in front of the real transport, so the test that matters most
// is that a real client still completes a full session through it. The
// handshake is where an over-eager interception would show up.
func TestMCPProbeDoesNotBreakRealSession(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "krabby", Version: "test"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping", Description: "test tool"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})

	sdk := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{},
	)

	ts := httptest.NewServer(mcpProbe(sdk))
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "probe-test", Version: "1"}, nil)

	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("connect through the guard: %v", err)
	}
	defer sess.Close()

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(tools.Tools) != 1 || tools.Tools[0].Name != "ping" {
		t.Fatalf("tools = %+v", tools.Tools)
	}

	out, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	txt, ok := out.Content[0].(*mcp.TextContent)
	if !ok || txt.Text != "pong" {
		t.Fatalf("result = %+v", out.Content)
	}
}

// sdkStub stands in for the MCP handler and records whether it was reached.
type sdkStub struct{ reached bool }

func (s *sdkStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.reached = true
	// Mirror what the real SDK does to a request it cannot serve, so a test
	// that expects passthrough can tell it happened.
	http.Error(w, "reached sdk", http.StatusBadRequest)
}

// Probes that previously got a 400 must now report the server as alive. These
// are the exact shapes observed from real health checks.
func TestMCPProbeAnswersLivenessChecks(t *testing.T) {
	cases := []struct {
		name   string
		method string
		accept string
	}{
		{"kubernetes httpGet probe sends no Accept", http.MethodGet, ""},
		{"curl and wget send */*", http.MethodGet, "*/*"},
		{"a browser sends text/html", http.MethodGet, "text/html,application/xhtml+xml"},
		{"a json client sends application/json", http.MethodGet, "application/json"},
		{"HEAD is never MCP traffic", http.MethodHead, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &sdkStub{}
			req := httptest.NewRequest(tc.method, "/mcp", nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}

			rec := httptest.NewRecorder()
			mcpProbe(stub).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, strings.TrimSpace(rec.Body.String()))
			}

			if stub.reached {
				t.Error("probe was forwarded to the MCP handler")
			}

			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}

			if tc.method == http.MethodHead {
				if rec.Body.Len() != 0 {
					t.Errorf("HEAD returned a body of %d bytes", rec.Body.Len())
				}

				return
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v (%q)", err, rec.Body.String())
			}

			if body["status"] != "ok" {
				t.Errorf("status field = %q", body["status"])
			}
			if body["service"] == "" || body["version"] == "" {
				t.Errorf("response does not identify the server: %v", body)
			}
		})
	}
}

// The guard must not swallow anything the MCP transport actually uses. A
// regression here would break clients rather than probes, so it is the more
// important of the two tests.
func TestMCPProbePassesProtocolTraffic(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		session string
	}{
		{"POST carries every JSON-RPC call", http.MethodPost, ""},
		{"POST within a session", http.MethodPost, "sess-1"},
		{"DELETE tears a session down", http.MethodDelete, "sess-1"},
		{"GET opens the SSE stream of a live session", http.MethodGet, "sess-1"},
		{"HEAD with a session id is left to the SDK", http.MethodHead, "sess-1"},
		{"unknown methods keep the SDK's 405", http.MethodPut, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &sdkStub{}
			req := httptest.NewRequest(tc.method, "/mcp", strings.NewReader("{}"))
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Content-Type", "application/json")
			if tc.session != "" {
				req.Header.Set("Mcp-Session-Id", tc.session)
			}

			rec := httptest.NewRecorder()
			mcpProbe(stub).ServeHTTP(rec, req)

			if !stub.reached {
				t.Fatalf("request was intercepted by the probe guard (status %d)", rec.Code)
			}
		})
	}
}

// Header casing is the client's choice; Go canonicalises it, and the guard must
// rely on that rather than on an exact match.
func TestMCPProbeSessionHeaderCasing(t *testing.T) {
	for _, name := range []string{"Mcp-Session-Id", "MCP-Session-ID", "mcp-session-id"} {
		stub := &sdkStub{}
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.Header.Set(name, "sess-1")

		mcpProbe(stub).ServeHTTP(httptest.NewRecorder(), req)

		if !stub.reached {
			t.Errorf("session header %q was not recognised", name)
		}
	}
}

// A configured API key must still shield the endpoint: the probe response is
// only reachable once the key check has passed. Otherwise the guard would turn
// an authenticated endpoint into an open one.
func TestMCPProbeStaysBehindAPIKey(t *testing.T) {
	const key = "secret"

	handler := apiKeyMiddleware(func() string { return key })(mcpProbe(&sdkStub{}))

	t.Run("without the key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))

		// 401 is itself a fine liveness signal: the server answered.
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("with the key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.Header.Set("X-Api-Key", key)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}
