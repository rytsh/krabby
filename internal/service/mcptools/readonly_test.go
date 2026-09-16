package mcptools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rytsh/krabby/internal/service/manager"
)

func TestReadOnlyCatalogsCannotMutate(t *testing.T) {
	mgr := &manager.Manager{}
	mgr.SetReadOnly(true)
	for name, server := range map[string]*mcp.Server{"api": NewAPI(mgr, "test"), "admin": NewAdmin(mgr, "test", 0)} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ct, st := mcp.NewInMemoryTransports()
			ss, err := server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			s, err := client.Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			tools, err := s.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if name == "api" {
				want = 4
			}
			if len(tools.Tools) != want {
				t.Fatalf("tools=%d want %d", len(tools.Tools), want)
			}
			for _, tool := range tools.Tools {
				if tool.Name == "call_api_endpoint" {
					t.Fatal("live API calls exposed")
				}
			}
			if _, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "call_api_endpoint", Arguments: map[string]any{"service": "x", "endpoint": "delete"}}); err == nil {
				t.Fatal("unadvertised mutation tool callable")
			}
		})
	}
}
