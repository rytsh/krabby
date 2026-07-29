package server

import (
	"encoding/json"
	"net/http"

	"github.com/rytsh/krabby/internal/config"
)

// mcpProbe answers requests to the MCP endpoint that cannot be MCP traffic,
// so a health check aimed at that path reports the server as alive.
//
// Why this exists. The MCP Streamable HTTP transport only speaks JSON-RPC over
// POST; a GET is reserved for opening the server-to-client SSE stream of an
// already-established session. Every naive probe therefore fails the
// transport's preconditions and is answered 400 by the SDK:
//
//	GET, no Accept header (the Kubernetes httpGet probe, Go's http.Get)
//	  -> 400 "Accept must contain 'text/event-stream' for GET requests"
//	GET, Accept: */* (curl, wget)
//	  -> 400 "Bad Request: GET requires an Mcp-Session-Id header"
//	HEAD
//	  -> 400 "Accept must contain both 'application/json' and 'text/event-stream'"
//
// Each of those is correct per the transport spec, and each is useless as a
// liveness signal: 400 reads as "this endpoint is broken" to a prober that
// cannot tell a malformed request from a malfunctioning server. A 401 does not
// have that problem, which is why an authenticated MCP endpoint probes fine
// today and only the open one misreports.
//
// What is intercepted. Only GET and HEAD *without* an Mcp-Session-Id header.
// That header is present on every GET belonging to a live session — the SDK
// rejects a session GET that lacks it — so this removes no working behaviour:
// the requests it catches are exactly the ones that could only ever have been
// answered with an error. POST (all JSON-RPC), DELETE (session teardown) and
// any GET carrying a session id are passed through untouched.
//
// Note this sits behind the API key middleware. When a key is configured an
// unauthenticated probe still gets 401 and never reaches here, which is the
// correct answer and already probe-friendly.
//
// A dedicated /healthz is still the better thing to point a health check at;
// this only stops the MCP path from lying about its own state when it is what
// somebody probed.
func mcpProbe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isProbe(r) {
			next.ServeHTTP(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		// Advertise what the endpoint actually accepts, so the reply is
		// self-describing rather than merely non-failing.
		w.Header().Set("Allow", "POST, GET, DELETE")
		// Probing must never be served from a cache: the point is to observe
		// the process right now.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)

		if r.Method == http.MethodHead {
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":    "ok",
			"service":   config.ServiceName,
			"version":   config.Version,
			"transport": "mcp-streamable-http",
			"detail":    "MCP endpoint is live. Protocol traffic is JSON-RPC over POST; this response is for probes only and is not an MCP message.",
		})
	})
}

// isProbe reports whether a request is a liveness probe rather than MCP
// traffic. See mcpProbe for why the session header is the discriminator.
func isProbe(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	// Header name per the Streamable HTTP transport. Go canonicalises it, so
	// the casing a client sends does not matter.
	return r.Header.Get("Mcp-Session-Id") == ""
}
