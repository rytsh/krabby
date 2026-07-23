package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/coderag"
)

func TestToolProfiles(t *testing.T) {
	tests := []struct {
		profile string
		count   int
		admin   bool
	}{
		{profile: ToolProfileStandard, count: 24},
		{profile: ToolProfileFull, count: 40, admin: true},
	}

	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			server := New(nil, "test", 0, tt.profile)
			ct, st := mcp.NewInMemoryTransports()
			if _, err := server.Connect(context.Background(), st, nil); err != nil {
				t.Fatal(err)
			}

			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			session, err := client.Connect(context.Background(), ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()

			result, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Tools) != tt.count {
				t.Fatalf("tool count = %d, want %d", len(result.Tools), tt.count)
			}
			if tt.profile == ToolProfileStandard {
				raw, err := json.Marshal(result.Tools)
				if err != nil {
					t.Fatal(err)
				}
				if len(raw) > 30_000 {
					t.Fatalf("standard tools/list payload grew to %d bytes", len(raw))
				}
			}

			names := map[string]bool{}
			for _, tool := range result.Tools {
				names[tool.Name] = true
			}
			for _, name := range []string{"search_code", "query_graph", "search_docs", "list_files"} {
				if !names[name] {
					t.Errorf("profile missing core tool %q", name)
				}
			}
			for _, name := range []string{"set_docs_config", "test_llm", "list_credentials", "lock_repo", "add_source", "refresh_source", "source_types"} {
				if names[name] != tt.admin {
					t.Errorf("admin tool %q present=%t, want %t", name, names[name], tt.admin)
				}
			}
		})
	}
}

func TestModelGuidanceIsSearchFirstAndBounded(t *testing.T) {
	if len(serverInstructions) > 1800 {
		t.Fatalf("server instructions grew to %d bytes", len(serverInstructions))
	}
	for _, phrase := range []string{"Use search_code first", "Use list_* only", "Always pass repo"} {
		if !strings.Contains(serverInstructions, phrase) {
			t.Errorf("instructions missing %q", phrase)
		}
	}
	if strings.Contains(serverInstructions, "best first call") {
		t.Fatal("instructions still recommend query_graph as a universal first call")
	}
}

func TestProbeSchemasContainOnlyRelevantFields(t *testing.T) {
	tests := []struct {
		name      string
		forSchema func(*jsonschema.ForOptions) (*jsonschema.Schema, error)
		max       int
	}{
		{"test_llm", jsonschema.For[testLLMArgs], 4},
		{"test_embedder", jsonschema.For[testEmbedderArgs], 7},
		{"test_code_embedder", jsonschema.For[testCodeEmbedderArgs], 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema, err := tt.forSchema(nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(schema.Properties) != tt.max {
				t.Fatalf("schema has %d properties, want %d", len(schema.Properties), tt.max)
			}
			if _, ok := schema.Properties["rag_top_k"]; ok {
				t.Fatal("probe schema leaked unrelated RAG settings")
			}
		})
	}
}

func TestJSONResultIsCompact(t *testing.T) {
	result := jsonResult(map[string]any{"a": 1, "b": []int{2, 3}})
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "\n") || strings.Contains(text, "  ") {
		t.Fatalf("JSON result is not compact: %q", text)
	}
	if !json.Valid([]byte(text)) {
		t.Fatalf("invalid JSON result: %q", text)
	}
}

func TestPageSliceBounds(t *testing.T) {
	page := pageSlice([]int{1, 2, 3, 4, 5}, 2, 2, 50)
	if len(page.Items) != 2 || page.Items[0] != 3 || !page.HasMore || page.Total != 5 {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestCodeSnippetsAreBounded(t *testing.T) {
	snippets := boundedCodeSnippets([]coderag.Snippet{{Snippet: strings.Repeat("x", 5000)}})
	if got := len([]rune(snippets[0].Snippet)); got != 4000 {
		t.Fatalf("snippet length = %d, want 4000", got)
	}
}
