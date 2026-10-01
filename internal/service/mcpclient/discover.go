package mcpclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxCatalogItems = 500
const maxResponseBytes = 4 << 20
const maxDiscoveryBytes = 8 << 20

// Discovery is a live catalog, not a snapshot of resource contents. Remote
// descriptions and schemas are untrusted data, never generation instructions.
type Discovery struct {
	OK                bool                    `json:"ok"`
	Error             string                  `json:"error,omitempty"`
	LatencyMS         int64                   `json:"latency_ms"`
	Server            *mcp.Implementation     `json:"server,omitempty"`
	ProtocolVersion   string                  `json:"protocol_version,omitempty"`
	Tools             []*mcp.Tool             `json:"tools"`
	Resources         []*mcp.Resource         `json:"resources"`
	ResourceTemplates []*mcp.ResourceTemplate `json:"resource_templates"`
	Truncated         bool                    `json:"truncated"`
}

// Discover only initializes and lists catalogs. It never calls a tool, reads a
// resource, follows resource URLs or enables sampling/elicitation.
func Discover(ctx context.Context, c *Connection) (result Discovery) {
	start := time.Now()
	result.Tools = []*mcp.Tool{}
	result.Resources = []*mcp.Resource{}
	result.ResourceTemplates = []*mcp.ResourceTemplate{}
	defer func() { result.LatencyMS = time.Since(start).Milliseconds() }()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	transport := &authTransport{base: base, connection: c}
	transport.remaining.Store(maxDiscoveryBytes)
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(c.TimeoutSeconds) * time.Second,
		// Even same-host redirects may point at a less trusted application.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "krabby-external", Version: "1"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: c.URL, HTTPClient: httpClient, MaxRetries: -1,
		DisableStandaloneSSE: true, MaxEventSize: maxResponseBytes,
	}, nil)
	if err != nil {
		result.Error = discoveryError(ctx, "initialize")
		return
	}
	defer func() { _ = session.Close() }()
	init := session.InitializeResult()
	result.Server, result.ProtocolVersion = init.ServerInfo, init.ProtocolVersion
	// Respect advertised capabilities: tools-only and resources-only servers
	// need not implement the other catalog's methods.
	if init.Capabilities.Tools != nil {
		items, truncated, err := listCatalog(ctx, func(cursor string) ([]*mcp.Tool, string, error) {
			page, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
			if err != nil {
				return nil, "", err
			}
			return page.Tools, page.NextCursor, nil
		})
		if err != nil {
			result.Error = discoveryError(ctx, "list tools")
			return
		}
		result.Tools, result.Truncated = items, truncated
	}
	if init.Capabilities.Resources != nil {
		items, truncated, err := listCatalog(ctx, func(cursor string) ([]*mcp.Resource, string, error) {
			page, err := session.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cursor})
			if err != nil {
				return nil, "", err
			}
			return page.Resources, page.NextCursor, nil
		})
		if err != nil {
			result.Error = discoveryError(ctx, "list resources")
			return
		}
		result.Resources, result.Truncated = items, result.Truncated || truncated
		templates, truncated, err := listCatalog(ctx, func(cursor string) ([]*mcp.ResourceTemplate, string, error) {
			page, err := session.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{Cursor: cursor})
			if err != nil {
				return nil, "", err
			}
			return page.ResourceTemplates, page.NextCursor, nil
		})
		if err != nil {
			result.Error = discoveryError(ctx, "list resource templates")
			return
		}
		result.ResourceTemplates, result.Truncated = templates, result.Truncated || truncated
	}
	result.OK = true
	return
}

// Bound both entries and page count, including peers that endlessly return
// empty pages or recycle cursors. Never claim that a partial catalog is complete.
func listCatalog[T any](ctx context.Context, fetch func(string) ([]T, string, error)) ([]T, bool, error) {
	items := []T{}
	cursor := ""
	seen := map[string]bool{"": true}
	for range 50 {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		page, next, err := fetch(cursor)
		if err != nil {
			return nil, false, err
		}
		remaining := maxCatalogItems - len(items)
		if len(page) > remaining {
			return append(items, page[:remaining]...), true, nil
		}
		items = append(items, page...)
		if next == "" {
			return items, false, nil
		}
		if len(items) == maxCatalogItems || seen[next] {
			return items, true, nil
		}
		seen[next], cursor = true, next
	}
	return items, true, nil
}

// Never return SDK errors or remote error messages: they may echo credentials,
// request headers or arbitrary content supplied by the external server.
func discoveryError(ctx context.Context, stage string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "MCP discovery timed out"
	}
	if ctx.Err() != nil {
		return "MCP discovery cancelled"
	}
	return "MCP discovery failed during " + stage + "; check the endpoint, authentication and Streamable HTTP support"
}

type authTransport struct {
	base       http.RoundTripper
	connection *Connection
	remaining  atomic.Int64
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	for key, value := range t.connection.Headers {
		r.Header.Set(key, value)
	}
	if t.connection.BearerToken != "" {
		r.Header.Set("Authorization", "Bearer "+t.connection.BearerToken)
	}
	res, err := t.base.RoundTrip(r)
	if err == nil {
		res.Body = &limitedBody{ReadCloser: res.Body, remaining: maxResponseBytes, total: &t.remaining}
	}
	return res, err
}

type limitedBody struct {
	io.ReadCloser
	remaining int64
	total     *atomic.Int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 || b.total.Load() <= 0 {
		return 0, errors.New("MCP response exceeds size limit")
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	if total := b.total.Load(); int64(len(p)) > total {
		p = p[:total]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	b.total.Add(-int64(n))
	return n, err
}
