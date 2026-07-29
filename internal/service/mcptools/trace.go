package mcptools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/manager"
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
func traceMiddleware(mgr *manager.Manager) mcp.Middleware {
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
				Input:     string(call.Params.Arguments),
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
