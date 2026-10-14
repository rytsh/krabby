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

// maxSyncBytes bounds everything one resource sync may download.
const maxSyncBytes = 512 << 20

// ResourceDoc is one granted resource read for indexing.
type ResourceDoc struct {
	Resource *mcp.Resource
	Contents []*mcp.ResourceContents
	Err      error
}

// SyncResources reads every resource granted on the connection (exact URI
// grants only, as for ReadResource) and passes it to fn, enriched with the
// metadata of the server's resource listing when it is listed there. include
// may narrow the granted set further.
func (s *Store) SyncResources(ctx context.Context, name string, include func(uri string) bool, fn func(ResourceDoc) error) error {
	c, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	if c.Disabled {
		return errors.New("MCP connection is disabled")
	}
	granted := map[string]bool{}
	for _, uri := range c.AllowedResources {
		if !strings.ContainsAny(uri, "{}") {
			granted[uri] = true
		}
	}
	if len(granted) == 0 {
		return errors.New("MCP connection grants no resources; select resources in the connection settings")
	}

	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	transport := &authTransport{base: base, connection: c}
	transport.remaining.Store(maxSyncBytes)
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	client := mcp.NewClient(&mcp.Implementation{Name: "krabby-source", Version: "1"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	session, err := client.Connect(connectCtx, &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: maxResponseBytes}, nil)
	cancel()
	if err != nil {
		return errors.New("MCP initialization failed; check the endpoint and authentication")
	}
	defer func() { _ = session.Close() }()

	listed := map[string]*mcp.Resource{}
	if session.InitializeResult().Capabilities.Resources != nil {
		listCtx, cancel := context.WithTimeout(ctx, timeout)
		items, _, err := listCatalog(listCtx, func(cursor string) ([]*mcp.Resource, string, error) {
			page, err := session.ListResources(listCtx, &mcp.ListResourcesParams{Cursor: cursor})
			if err != nil {
				return nil, "", err
			}
			return page.Resources, page.NextCursor, nil
		})
		cancel()
		if err != nil {
			return errors.New("MCP list resources failed")
		}
		for _, r := range items {
			listed[r.URI] = r
		}
	}

	uris := make([]string, 0, len(granted))
	for uri := range granted {
		if include == nil || include(uri) {
			uris = append(uris, uri)
		}
	}
	slices.Sort(uris)

	for _, uri := range uris {
		if err := ctx.Err(); err != nil {
			return err
		}
		res := listed[uri]
		if res == nil {
			res = &mcp.Resource{URI: uri}
		}
		readCtx, cancel := context.WithTimeout(ctx, timeout)
		result, err := session.ReadResource(readCtx, &mcp.ReadResourceParams{URI: uri})
		cancel()
		doc := ResourceDoc{Resource: res}
		if err != nil {
			doc.Err = errors.New("MCP resource read failed")
		} else {
			doc.Contents = result.Contents
		}
		if err := fn(doc); err != nil {
			return err
		}
	}

	return nil
}
