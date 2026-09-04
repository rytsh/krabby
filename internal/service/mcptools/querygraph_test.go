package mcptools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/graphquery"
)

func TestQueryGraphArgsTraversalMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    string
		want    string
		wantErr bool
	}{
		{name: "empty is the default", want: "bfs"},
		{name: "bfs", mode: "bfs", want: "bfs"},
		{name: "dfs", mode: "dfs", want: "dfs"},
		// These used to fall through to BFS and be reported as the requested
		// traversal, so the caller could not tell the answer was not a trace.
		{name: "long form", mode: "depth-first", wantErr: true},
		{name: "upper case", mode: "DFS", wantErr: true},
		{name: "typo", mode: "dsf", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := queryGraphArgs{Mode: tt.mode}.traversalMode()
			if (err != nil) != tt.wantErr {
				t.Fatalf("traversalMode(%q) error = %v, wantErr %v", tt.mode, err, tt.wantErr)
			}
			if err != nil {
				for _, want := range []string{"bfs", "dfs"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name the accepted mode %q", err, want)
					}
				}

				return
			}
			if got != tt.want {
				t.Fatalf("traversalMode(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

// stubGraphQuery records the graph tool calls that made it past argument
// validation.
type stubGraphQuery struct {
	calls []map[string]any
}

func (s *stubGraphQuery) CallGraphTool(_ context.Context, _, _, _ string, args map[string]any) (*mcp.CallToolResult, error) {
	s.calls = append(s.calls, args)

	return textResult("Traversal: BFS depth=3"), nil
}

func (s *stubGraphQuery) FindDefinition(context.Context, string, string, string, int) (graphquery.SymbolDefs, error) {
	return graphquery.SymbolDefs{}, nil
}

func (s *stubGraphQuery) FindReferences(context.Context, string, string, string, []string, int, int) (graphquery.SymbolRefs, error) {
	return graphquery.SymbolRefs{}, nil
}

func queryToolSession(t *testing.T) (*mcp.ClientSession, *stubGraphQuery) {
	t.Helper()

	stub := &stubGraphQuery{}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	addQueryTools(server, stub)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session, stub
}

// An unknown mode must be refused rather than answered: the traversal defaults
// to BFS, so mode='depth-first' otherwise returns a plausible breadth-first
// subgraph under a header claiming it is the trace the caller asked for.
func TestQueryGraphToolRejectsUnknownMode(t *testing.T) {
	session, stub := queryToolSession(t)

	text, isErr := callToolText(t, session, "query_graph", map[string]any{
		"question": "how does the manager reach the registry", "mode": "depth-first",
	})
	if !isErr {
		t.Fatalf("query_graph accepted mode='depth-first': %s", text)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("query_graph reached the graph engine with %+v", stub.calls)
	}
	for _, want := range []string{"bfs", "dfs"} {
		if !strings.Contains(text, want) {
			t.Errorf("error %q does not name the accepted mode %q", text, want)
		}
	}
}

// Empty must keep meaning "use the default traversal": the check rejects typos,
// not the omission of an optional argument.
func TestQueryGraphToolRunsDefaultTraversalWithoutMode(t *testing.T) {
	session, stub := queryToolSession(t)

	for _, mode := range []string{"", "bfs", "dfs"} {
		args := map[string]any{"question": "how does the manager reach the registry"}
		if mode != "" {
			args["mode"] = mode
		}

		text, isErr := callToolText(t, session, "query_graph", args)
		if isErr {
			t.Fatalf("query_graph(mode=%q) failed: %s", mode, text)
		}
	}

	if len(stub.calls) != 3 {
		t.Fatalf("graph engine calls = %d, want 3", len(stub.calls))
	}
	if _, ok := stub.calls[0]["mode"]; ok {
		t.Fatalf("an omitted mode was forwarded: %+v", stub.calls[0])
	}
	if got := stub.calls[2]["mode"]; got != "dfs" {
		t.Fatalf("forwarded mode = %v, want dfs", got)
	}
}
