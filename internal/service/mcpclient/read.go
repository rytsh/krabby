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

// ReadResource only accepts an exact, currently saved URI grant. URI templates
// are not expanded implicitly and no tool, sampling or elicitation is enabled.
// Revocation is effective for subsequent reads, not an in-flight request.
func (s *Store) ReadResource(ctx context.Context, name, uri string, maxBytes int) (string, bool, error) {
	c, err := s.Get(ctx, name)
	if err != nil {
		return "", false, err
	}
	if c.Disabled || strings.ContainsAny(uri, "{}") || !slices.Contains(c.AllowedResources, uri) {
		return "", false, errors.New("MCP resource not granted or connection disabled")
	}
	if maxBytes <= 0 || maxBytes > 32<<10 {
		maxBytes = 32 << 10
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	transport := &authTransport{base: base, connection: c}
	transport.remaining.Store(maxDiscoveryBytes)
	httpClient := &http.Client{Transport: transport, Timeout: time.Duration(c.TimeoutSeconds) * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	client := mcp.NewClient(&mcp.Implementation{Name: "krabby-research", Version: "1"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: maxResponseBytes}, nil)
	if err != nil {
		return "", false, errors.New("MCP resource initialization failed")
	}
	defer func() { _ = session.Close() }()
	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return "", false, errors.New("MCP resource read failed")
	}
	var text strings.Builder
	truncated := false
	for _, content := range result.Contents {
		if len(content.Blob) != 0 {
			truncated = true
			continue
		}
		remaining := maxBytes - text.Len()
		value := content.Text
		if len(value) > remaining {
			value = value[:remaining]
			truncated = true
		}
		text.WriteString(value)
	}
	return strings.ToValidUTF8(text.String(), ""), truncated, nil
}
