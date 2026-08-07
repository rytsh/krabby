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

// toolsPayloadBudget bounds the standard profile's tools/list response. Every
// MCP session receives it in full before any work starts, and it occupies the
// model's context whether or not a tool is used — provider-side prompt caching
// lowers the price but not the occupancy.
//
// The budget is expressed in bytes because that is what the test can measure
// without a tokenizer, but the number that matters is tokens. Measured with
// tiktoken over the actual payload:
//
//	39,290 B  -> roughly 8,000 tokens after adding the API-catalog tools
//	 2,585 B  ->    527 tokens  (serverInstructions, on top of this)
//
// That is ~4.9 bytes per token, better than plain prose, because the payload is
// mostly repeated JSON scaffolding and ordinary English — BPE collapses both.
// So this budget is roughly 8,500 tokens, ~4% of a 200k context and ~27% of a
// 32k one, paid on every session.
//
// Raising it is a decision, not a formality: check first whether a tool
// description is carrying detail that belongs in the handler's error messages,
// or whether a jsonschema field is explaining nuance that only matters once a
// call is being made.
//
// It was last raised from 35,000 for the four API-catalog discovery tools. The
// alternative considered was one drill-down tool taking a widening set of
// arguments, which would have cost ~1.5 KB instead of ~6.3 KB — but it makes
// the progressive-disclosure contract implicit in an argument combination
// rather than explicit in four names and four descriptions, and a model that
// misreads it fetches an entire specification. The names are the guardrail, so
// they were worth the bytes.
const (
	toolsPayloadBudget = 42_000
	// bytesPerToken is the measured ratio above, for reporting only.
	bytesPerToken = 5
)

func TestToolProfiles(t *testing.T) {
	tests := []struct {
		profile string
		count   int
		admin   bool
	}{
		{profile: ToolProfileStandard, count: 38},
		{profile: ToolProfileFull, count: 64, admin: true},
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
				if len(raw) > toolsPayloadBudget {
					t.Fatalf("standard tools/list payload grew to %d bytes (~%d tokens), budget %d",
						len(raw), len(raw)/bytesPerToken, toolsPayloadBudget)
				}
			}

			names := map[string]bool{}
			for _, tool := range result.Tools {
				names[tool.Name] = true
				if (tool.Name == "search_docs" || tool.Name == "list_sources" || tool.Name == "list_namespaces" || tool.Name == "get_source" ||
					tool.Name == "register_source_page" || tool.Name == "import_source_pages" || tool.Name == "import_source_sitemap" || tool.Name == "delete_source_page" ||
					tool.Name == "get_source_config") && tool.OutputSchema == nil {
					t.Errorf("discovery tool %q has no output schema", tool.Name)
				}
				rawSchema, err := json.Marshal(tool.OutputSchema)
				if err != nil {
					t.Fatal(err)
				}
				schemaText := string(rawSchema)
				switch tool.Name {
				case "search_docs":
					for _, field := range []string{"source_kind", "scope_key", "namespace", "collection_name"} {
						if !strings.Contains(schemaText, field) {
							t.Errorf("search_docs output schema missing %q", field)
						}
					}
				case "get_source":
					if !strings.Contains(schemaText, `"items"`) || strings.Contains(schemaText, `"config"`) || strings.Contains(schemaText, `"last_error"`) {
						t.Errorf("get_source output schema is not the bounded discovery DTO: %s", schemaText)
					}
				case "set_docs_config":
					for _, field := range []string{"web_image_model", "web_image_analysis_enabled", "rag_keep_markdown_targets"} {
						input, _ := json.Marshal(tool.InputSchema)
						if !strings.Contains(string(input), field) {
							t.Errorf("set_docs_config input schema missing %q", field)
						}
					}
				case "add_source", "update_source":
					input, _ := json.Marshal(tool.InputSchema)
					if !strings.Contains(string(input), "analyze_images") {
						t.Errorf("%s input schema missing analyze_images", tool.Name)
					}
				}
			}
			for _, name := range []string{"search_code", "query_graph", "search_docs", "list_files", "get_source"} {
				if !names[name] {
					t.Errorf("profile missing core tool %q", name)
				}
			}
			// Queue-management tools are available in both profiles.
			for _, name := range []string{"queue_status", "bump_task", "cancel_task", "set_task_concurrency"} {
				if !names[name] {
					t.Errorf("profile missing queue tool %q", name)
				}
			}
			for _, name := range []string{
				"set_docs_config", "test_llm", "list_credentials", "add_source", "refresh_source", "source_types", "get_source_config",
				"register_source_page", "import_source_pages", "import_source_sitemap", "delete_source_page",
			} {
				if names[name] != tt.admin {
					t.Errorf("admin tool %q present=%t, want %t", name, names[name], tt.admin)
				}
			}
			for _, name := range []string{"lock_repo", "unlock_repo"} {
				if names[name] {
					t.Errorf("removed lease tool %q is still registered", name)
				}
			}
		})
	}
}

// serverInstructionsBudget bounds the server-level guidance. Every MCP session
// pays for it in full, so it must stay a tool-selection map and never grow into
// per-tool documentation, which belongs in each tool's Description.
//
// 2,700 bytes is roughly 540 tokens (see toolsPayloadBudget for the measured
// bytes-per-token ratio), on top of the tools/list payload.
const serverInstructionsBudget = 2700

func TestModelGuidanceIsSearchFirstAndBounded(t *testing.T) {
	if len(serverInstructions) > serverInstructionsBudget {
		t.Fatalf("server instructions grew to %d bytes (~%d tokens), budget %d",
			len(serverInstructions), len(serverInstructions)/bytesPerToken, serverInstructionsBudget)
	}
	for _, phrase := range []string{"Use search_code first", "Use list_* only", "Always pass repo"} {
		if !strings.Contains(serverInstructions, phrase) {
			t.Errorf("instructions missing %q", phrase)
		}
	}
	// The catalog chain has to be named here rather than left to the tool
	// descriptions: a model that never calls list_api_groups never reads them,
	// and answers "how do I call X" out of prose it found with search_docs.
	if !strings.Contains(serverInstructions, "list_api_groups -> list_api_services -> list_api_endpoints -> get_api_endpoint") {
		t.Error("instructions do not describe the API-catalog drill-down order")
	}
	if !strings.Contains(serverInstructions, "Semantic is the default when configured") {
		t.Fatal("instructions do not describe the effective docs-search default")
	}
	if strings.Contains(serverInstructions, "Hybrid mode is the default") {
		t.Fatal("instructions still claim hybrid is the default")
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
