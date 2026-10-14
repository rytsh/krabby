package mcptools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/memory"
)

type authorMemoryService struct {
	memoryService
	authors chan string
}

func (s *authorMemoryService) Remember(_ context.Context, in memory.Input) (manager.RememberResult, error) {
	s.authors <- in.Author
	return manager.RememberResult{Memory: &memory.Memory{ID: "test"}}, nil
}

func (s *authorMemoryService) UpdateMemory(_ context.Context, _ string, patch memory.Patch) (*memory.Memory, error) {
	s.authors <- patch.Author
	return &memory.Memory{ID: "test"}, nil
}

type memoryUserTransport struct {
	user string
}

func (t memoryUserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("X-User", t.user)
	return http.DefaultTransport.RoundTrip(req)
}

func TestMemoryAuthorHTTP(t *testing.T) {
	for _, tt := range []struct {
		name   string
		user   string
		author string
		want   string
	}{
		{name: "header fallback", user: "Eray", want: "Eray"},
		{name: "explicit author wins", user: "Eray", author: "Agent", want: "Agent"},
		{name: "blank author", user: "Eray", author: "  ", want: "Eray"},
		{name: "no author or header"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			svc := &authorMemoryService{authors: make(chan string, 1)}
			server := mcp.NewServer(&mcp.Implementation{Name: "memory-test", Version: "test"}, nil)
			addMemoryTools(server, svc)
			ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
			defer ts.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint: ts.URL, HTTPClient: &http.Client{Transport: memoryUserTransport{user: tt.user}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session.Close() }()
			for _, tool := range []string{"remember", "update_memory"} {
				args := map[string]any{"author": tt.author}
				if tool == "remember" {
					args["title"], args["content"] = "Decision", "Use the header."
				} else {
					args["id"] = "test"
				}
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				if result.IsError {
					t.Fatalf("%s failed: %+v", tool, result)
				}
				if got := <-svc.authors; got != tt.want {
					t.Errorf("%s author = %q, want %q", tool, got, tt.want)
				}
			}
		})
	}
}

func TestMemoryAuthorWithoutHTTP(t *testing.T) {
	for _, req := range []*mcp.CallToolRequest{nil, {}, {Extra: &mcp.RequestExtra{}}} {
		if got := memoryAuthor("Agent", req); got != "Agent" {
			t.Errorf("explicit author = %q, want Agent", got)
		}
		if got := memoryAuthor("", req); got != "" {
			t.Errorf("missing author = %q, want empty", got)
		}
	}
}
