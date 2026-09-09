package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/repofs"
)

type researchToolsStub struct {
	docsSearchService
	readOffset int64
	readPath   string
}

func (*researchToolsStub) RepoOverview(_ context.Context, repo string) (manager.RepoOverview, error) {
	return manager.RepoOverview{Repo: repo, Available: true, Path: "documentation.md", Overview: "# Payments", Sections: []manager.OverviewSection{{Title: "Retry", Offset: 123}}, EvidenceKind: "generated_summary", Note: "Verify in source."}, nil
}

func (s *researchToolsStub) GetDocDetails(_ context.Context, key, path string, offset int64, _ int) (manager.DocumentRead, error) {
	s.readOffset, s.readPath = offset, path
	return manager.DocumentRead{FileContent: &repofs.FileContent{Path: path, Content: "## Retry", Bytes: 8, TotalSize: 131}, ScopeKey: key, SourceKind: "repository", Evidence: rag.DocEvidence{Kind: "generated_summary"}}, nil
}

func TestResearchToolsExposeNavigableGeneratedEvidence(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	stub := &researchToolsStub{}
	addDocTools(server, stub, nil)
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
	text, failed := callToolText(t, session, "repo_overview", map[string]any{"repo": "host/payments"})
	if failed {
		t.Fatal(text)
	}
	var overview manager.RepoOverview
	if err := json.Unmarshal([]byte(text), &overview); err != nil {
		t.Fatal(err)
	}
	if !overview.Available || len(overview.Sections) != 1 {
		t.Fatalf("overview=%s", text)
	}
	outputs := map[string]string{"repo_overview": text}
	text, failed = callToolText(t, session, "get_doc", map[string]any{"repo": overview.Repo, "path": overview.Path, "offset": overview.Sections[0].Offset})
	if failed || stub.readOffset != 123 || stub.readPath != overview.Path {
		t.Fatalf("read=%s offset=%d path=%s", text, stub.readOffset, stub.readPath)
	}
	var read manager.DocumentRead
	if err := json.Unmarshal([]byte(text), &read); err != nil {
		t.Fatal(err)
	}
	if read.Evidence.Kind != "generated_summary" || read.Content != "## Retry" {
		t.Fatalf("read=%s", text)
	}
	outputs["get_doc"] = text
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if text, ok := outputs[tool.Name]; ok {
			raw, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			var instance any
			if err := json.Unmarshal([]byte(text), &instance); err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(instance); err != nil {
				t.Fatalf("%s output violates its schema: %v", tool.Name, err)
			}
		}
		if tool.Name == "repo_overview" || tool.Name == "get_doc" || tool.Name == "search_docs" || tool.Name == "list_sources" || tool.Name == "get_source" {
			if tool.OutputSchema == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("%s lacks typed output/read-only annotation", tool.Name)
			}
		}
		if tool.Name == "get_doc" || tool.Name == "search_docs" {
			if !strings.Contains(tool.Description, "live") || !strings.Contains(tool.Description, "Krabby") {
				t.Errorf("ambiguous provider role: %s", tool.Description)
			}
		}
	}
}

func TestResearchGuidanceDistinguishesLiveProviders(t *testing.T) {
	for _, phrase := range []string{"dedicated Jira/Confluence MCP", "not live provider data", "original URL", "does not prove", "repo_overview", "Verify implementation claims in source"} {
		if !strings.Contains(serverInstructions, phrase) {
			t.Errorf("guidance missing %q", phrase)
		}
	}
	if !strings.Contains(adminInstructions, "not upstream Jira issues or Confluence pages") {
		t.Fatal("source administration is ambiguous")
	}
}
