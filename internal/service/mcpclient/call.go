package mcpclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolCall is one tool invocation requested by Big Picture research.
type ToolCall struct {
	Tool      string
	Arguments map[string]any
}

// ToolResult is the text output of one call. Err never carries remote error
// text, which may echo credentials or arbitrary server content.
type ToolResult struct {
	Text      string
	Truncated bool
	Err       error
}

// CallTools runs calls over one session. Only tools in the connection's
// current AllowedTools grant are executed; others fail individually. The
// whole batch shares one deadline so a slow server cannot stall research.
func (s *Store) CallTools(ctx context.Context, name string, calls []ToolCall, maxBytes int) ([]ToolResult, error) {
	c, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if c.Disabled {
		return nil, errors.New("MCP connection disabled")
	}
	if maxBytes <= 0 || maxBytes > 32<<10 {
		maxBytes = 32 << 10
	}
	results := make([]ToolResult, len(calls))
	runnable := false
	for i, call := range calls {
		if !slices.Contains(c.AllowedTools, call.Tool) {
			results[i].Err = errors.New("MCP tool not granted")
			continue
		}
		runnable = true
	}
	if !runnable {
		return results, nil
	}
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, min(5*time.Minute, timeout*time.Duration(len(calls)+1)))
	defer cancel()
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	transport := &authTransport{base: base, connection: c}
	transport.remaining.Store(maxDiscoveryBytes * 4)
	httpClient := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	client := mcp.NewClient(&mcp.Implementation{Name: "krabby-research", Version: "1"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: maxResponseBytes}, nil)
	if err != nil {
		return nil, errors.New("MCP tool session initialization failed")
	}
	defer func() { _ = session.Close() }()
	for i, call := range calls {
		if results[i].Err != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			results[i].Err = errors.New("MCP tool call skipped: time limit reached")
			continue
		}
		callCtx, callCancel := context.WithTimeout(ctx, timeout)
		res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: call.Tool, Arguments: call.Arguments})
		callCancel()
		if err != nil {
			results[i].Err = errors.New("MCP tool call failed")
			continue
		}
		if res.IsError {
			results[i].Err = errors.New("MCP tool reported an error")
			continue
		}
		var text strings.Builder
		for _, content := range res.Content {
			tc, ok := content.(*mcp.TextContent)
			if !ok {
				results[i].Truncated = true
				continue
			}
			value := tc.Text
			if remaining := maxBytes - text.Len(); len(value) > remaining {
				value = value[:remaining]
				results[i].Truncated = true
			}
			text.WriteString(value)
		}
		results[i].Text = strings.ToValidUTF8(text.String(), "")
	}
	return results, nil
}
