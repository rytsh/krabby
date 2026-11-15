package mcptools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/observability/langfuse"
)

// maxTracedResult caps how much of a tool result is attached to a span before
// the capture policy is even consulted. A search can return hundreds of
// kilobytes of source; the head of it is what makes a trace readable, the rest
// is weight on the export queue.
const maxTracedResult = 16 << 10

// traceMiddleware exports every MCP tool call as a Langfuse trace.
//
// The tracer is read from the manager on each call rather than captured once:
// the MCP server is built at startup, but observability settings are live, so
// a tracer captured here would be the one from whichever bundle happened to
// exist first.
//
// Only tools/call is traced. The rest of the MCP surface (initialize, listing,
// ping) is protocol chatter that says nothing about what an agent asked for
// and would multiply the observation count.
func traceMiddleware(mgr tracingService) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			tracer := mgr.Tracer()
			if method != "tools/call" || !tracer.On(langfuse.ScopeMCP) {
				return next(ctx, method, req)
			}

			call, ok := req.(*mcp.CallToolRequest)
			if !ok {
				return next(ctx, method, req)
			}

			name := call.Params.Name

			// The tool call is a root: an agent's request is its own unit of
			// work, and the HTTP span wrapping it belongs to the gRPC
			// collector, not to Langfuse.
			ctx, endTrace := tracer.StartTrace(ctx, langfuse.ScopeMCP, langfuse.TraceInfo{
				Name:      name,
				SessionID: sessionID(req),
				Tags:      []string{"krabby", "mcp"},
				Metadata:  map[string]string{"tool": name},
				Input:     redactArguments(call.Params.Arguments),
			})

			res, err := next(ctx, method, req)

			endTrace(toolOutput(res), err)

			return res, err
		}
	}
}

// sessionID returns the MCP session id when the transport provides one, so the
// tools one agent conversation invoked group together in Langfuse.
func sessionID(req mcp.Request) string {
	sess := req.GetSession()
	if sess == nil {
		return ""
	}

	return sess.ID()
}

// toolOutput renders the textual part of a tool result, clipped. Structured
// content is skipped: it duplicates the text and can be arbitrarily large.
func toolOutput(res mcp.Result) any {
	out, ok := res.(*mcp.CallToolResult)
	if !ok || out == nil {
		return nil
	}

	var text string
	for _, c := range out.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}

		text += tc.Text
		if len(text) >= maxTracedResult {
			text = text[:maxTracedResult] + "\n…[truncated]"

			break
		}
	}

	if text == "" {
		return nil
	}

	if out.IsError {
		return map[string]any{"error": true, "text": text}
	}

	return text
}

// redactedValue replaces a secret in a traced tool call.
const redactedValue = "[redacted]"

// redactArguments returns a tool call's arguments with every value that could
// be a credential replaced, for export to Langfuse.
//
// Admin tools take secrets as ordinary arguments: set_credential's secret,
// set_docs_config's API keys, an external MCP server's bearer token and
// headers, a source config's api_token. Tracing is on and captures full
// content by default, so those would otherwise leave for a third-party
// service in clear text on every call. A key counts as secret by its name,
// at any depth; headers are dropped whole because their names say nothing
// about which of them authenticate. Arguments that are not a JSON object are
// not traced at all.
func redactArguments(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return redactedValue
	}
	if _, ok := v.(map[string]any); !ok {
		return redactedValue
	}

	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return redactedValue
	}

	return string(out)
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if secretArgument(k) {
				if s, ok := val.(string); !ok || s != "" {
					t[k] = redactedValue
				}

				continue
			}
			t[k] = redactValue(val)
		}
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
	}

	return v
}

// secretArgument reports whether an argument name can hold a credential.
// Flags about secrets ("token_set", "clear_bearer_token") and budgets
// ("token_budget") are not secrets and stay readable.
func secretArgument(name string) bool {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, "_set"), strings.HasPrefix(n, "clear_"), n == "token_budget", n == "header_names":
		return false
	case n == "headers", n == "password", n == "secret", n == "token":
		return true
	}

	for _, part := range []string{"secret", "token", "api_key", "password", "access_key", "private_key", "authorization", "cookie", "credential"} {
		if strings.Contains(n, part) {
			return true
		}
	}

	return false
}
