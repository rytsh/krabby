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

// toolsPayloadBudget bounds the core catalog's tools/list response. Every
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

func TestToolCatalogs(t *testing.T) {
	tests := []struct {
		name    string
		server  func() *mcp.Server
		count   int
		present []string
		absent  []string
	}{
		{
			name: "core", server: func() *mcp.Server { return NewCore(nil, "test") }, count: 25,
			present: []string{"list_repos", "repo_status", "search_code", "query_graph", "find_definition", "find_references", "search_docs", "list_files", "glob", "get_source"},
			absent:  []string{"add_repo", "remove_repo", "refresh_repo", "queue_status", "call_api_endpoint", "set_docs_config"},
		},
		{
			name: "api", server: func() *mcp.Server { return NewAPI(nil, "test") }, count: 5,
			present: []string{"list_api_groups", "list_api_services", "list_api_endpoints", "get_api_endpoint", "call_api_endpoint"},
			absent:  []string{"search_code", "add_api_service", "set_docs_config", "add_repo"},
		},
		{
			name: "admin", server: func() *mcp.Server { return NewAdmin(nil, "test", 0) }, count: 38,
			present: []string{"add_repo", "remove_repo", "refresh_repo", "queue_status", "set_docs_config", "add_source", "add_api_service", "set_credential"},
			absent:  []string{"list_repos", "repo_status", "search_code", "query_graph", "search_docs", "list_api_services", "call_api_endpoint"},
		},
	}

	allNames := map[string]string{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := tt.server()
			ct, st := mcp.NewInMemoryTransports()
			if _, err := server.Connect(context.Background(), st, nil); err != nil {
				t.Fatal(err)
			}

			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			session, err := client.Connect(context.Background(), ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session.Close() }()

			result, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Tools) != tt.count {
				t.Fatalf("tool count = %d, want %d", len(result.Tools), tt.count)
			}
			if tt.name == "core" {
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
				if owner, exists := allNames[tool.Name]; exists {
					t.Errorf("tool %q is published by both %s and %s", tool.Name, owner, tt.name)
				}
				allNames[tool.Name] = tt.name
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
					for _, field := range []string{"source_kind", "scope_key", "namespace", "collection_name", "service_name"} {
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
			for _, name := range tt.present {
				if !names[name] {
					t.Errorf("catalog missing tool %q", name)
				}
			}
			for _, name := range tt.absent {
				if names[name] {
					t.Errorf("catalog unexpectedly publishes tool %q", name)
				}
			}
			for _, name := range []string{"lock_repo", "unlock_repo"} {
				if names[name] {
					t.Errorf("removed lease tool %q is still registered", name)
				}
			}
		})
	}
	if len(allNames) != 68 {
		t.Fatalf("catalog union has %d tools, want 68", len(allNames))
	}
}

// TestPublishedCallSchemaAcceptsJSONBody checks the schema a real client
// actually receives, not one the test built itself.
//
// The unit test in schema_test.go can only prove that forOptions produces the
// right shape; it cannot prove addTool applies it. This connects a client and
// validates a realistic request against the advertised input schema, which is
// exactly what a strict client does before sending — and what was rejecting
// every call_api_endpoint body as "has type object, want array".
func TestPublishedCallSchemaAcceptsJSONBody(t *testing.T) {
	server := NewAPI(nil, "test")
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var tool *mcp.Tool
	for _, candidate := range result.Tools {
		if candidate.Name == "call_api_endpoint" {
			tool = candidate

			break
		}
	}
	if tool == nil {
		t.Fatal("call_api_endpoint not published by the api catalog")
	}

	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	// The byte-array rendering is the specific regression being guarded.
	if strings.Contains(string(raw), `"maximum":255`) {
		t.Fatalf("body is still advertised as an array of bytes: %s", raw)
	}

	// Round-trip through the wire form, so the test validates against the same
	// bytes a client parses rather than the in-process value.
	var published jsonschema.Schema
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatalf("parse published schema: %v", err)
	}

	resolved, err := published.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve published schema: %v", err)
	}

	for _, body := range []string{
		`{"query":"limit=1"}`,
		`[{"query":"a"},{"query":"b"}]`,
		`"raw text"`,
	} {
		var args any
		payload := `{"service":"transaction","endpoint":"/v1.TransactionService/GetTransactions","body":` + body + `}`
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			t.Fatal(err)
		}
		if err := resolved.Validate(args); err != nil {
			t.Errorf("published schema rejects body %s: %v", body, err)
		}
	}
}

func TestAdminCatalogUsesTypedJSONObjects(t *testing.T) {
	server := NewAdmin(nil, "test", 0)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := make(map[string]*mcp.Tool, len(result.Tools))
	for _, tool := range result.Tools {
		tools[tool.Name] = tool
	}

	for _, name := range []string{"add_api_service", "update_api_service"} {
		tool := tools[name]
		if tool == nil {
			t.Fatalf("tool %q is missing", name)
		}
		var schema jsonschema.Schema
		raw, _ := json.Marshal(tool.InputSchema)
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode %s input schema: %v", name, err)
		}
		for _, field := range []string{"config", "spec_patch"} {
			property := schema.Properties[field]
			if property == nil {
				t.Errorf("%s input %s is missing", name, field)
			} else if property.Type != "object" {
				t.Errorf("%s input %s type = %q, want object", name, field, property.Type)
			}
		}
	}

	for _, name := range []string{"get_api_service_config", "get_source_config"} {
		tool := tools[name]
		if tool == nil || tool.OutputSchema == nil {
			t.Fatalf("tool %q or its output schema is missing", name)
		}
		var schema jsonschema.Schema
		raw, _ := json.Marshal(tool.OutputSchema)
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode %s output schema: %v", name, err)
		}
		property := schema.Properties["config"]
		if property == nil {
			t.Errorf("%s output config is missing", name)
		} else if property.Type != "object" {
			t.Errorf("%s output config type = %q, want object", name, property.Type)
		}
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
	// The catalog chain has to be named rather than left to the tool
	// descriptions: a model that never calls list_api_groups never reads them,
	// and answers "how do I call X" out of prose it found with search_docs. It
	// belongs in apiInstructions, not the base text, because the core catalog
	// publishes none of those tools.
	if !strings.Contains(apiInstructions, "list_api_groups -> list_api_services -> list_api_endpoints -> get_api_endpoint") {
		t.Error("api instructions do not describe the API-catalog drill-down order")
	}
	if strings.Contains(serverInstructions, "list_api_groups") {
		t.Error("base instructions describe API tools the core catalog does not publish")
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
