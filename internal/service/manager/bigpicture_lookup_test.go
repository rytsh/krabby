package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

type configArgs struct {
	Path   string `json:"path"`
	Source string `json:"source,omitempty"`
}

func TestBigPictureMCPLookups(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	pictures, err := bigpicture.New(db, filepath.Join(dir, "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	mcps, err := mcpclient.New(db)
	if err != nil {
		t.Fatal(err)
	}

	// A config server with a catalog of sections and a get_config tool.
	var mu sync.Mutex
	var requested []configArgs
	remote := mcp.NewServer(&mcp.Implementation{Name: "config", Version: "test"}, nil)
	remote.AddResource(&mcp.Resource{Name: "consul", URI: "config://consul"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	mcp.AddTool(remote, &mcp.Tool{Name: "get_config", Description: "Read configuration by path"}, func(_ context.Context, _ *mcp.CallToolRequest, args configArgs) (*mcp.CallToolResult, any, error) {
		mu.Lock()
		requested = append(requested, args)
		mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "kafka.topic: order-created\ndb.password: hunter2"}}}, nil, nil
	})
	mcp.AddTool(remote, &mcp.Tool{Name: "delete_config"}, func(context.Context, *mcp.CallToolRequest, configArgs) (*mcp.CallToolResult, any, error) {
		t.Error("ungranted tool executed")
		return &mcp.CallToolResult{}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	if _, err := mcps.Save(ctx, "", mcpclient.Config{Name: "config", URL: server.URL, AllowedTools: []string{"get_config"}}); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "orders")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("Orders service.\n\nconfig.go loads `admin/orders`."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reg.Upsert(ctx, &registry.Repo{ID: "acme/orders", Path: root, Status: "ready"}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{reg: reg, bigPictures: pictures, externalMCPs: mcps}
	p, err := m.SaveBigPicture(ctx, "", bigpicture.Config{Name: "acme", Title: "Acme", Prompt: "Look up the config paths in consul.", Sources: []bigpicture.Source{{Kind: "repo", Ref: "acme/orders"}, {Kind: "mcp", Ref: "config"}}})
	if err != nil {
		t.Fatal(err)
	}
	research, err := m.collectPictureResearch(ctx, p)
	if err != nil {
		t.Fatal(err)
	}

	planner := pictureCompletionFunc(func(_ context.Context, messages []llm.Message) (string, error) {
		var input struct {
			Tools   []bigpicture.LookupTool `json:"tools"`
			Catalog map[string][]string     `json:"server_catalog"`
		}
		_ = json.Unmarshal([]byte(messages[1].Content), &input)
		if len(input.Tools) != 1 || input.Tools[0].Name != "get_config" || input.Catalog["config"][0] != "consul" {
			t.Errorf("planner input: %+v", input)
		}
		return `{"calls":[
			{"server":"config","tool":"get_config","arguments":{"path":"admin/orders","source":"consul"},"reason":"orders config"},
			{"server":"config","tool":"get_config","arguments":{"path":"secret/prod/db"}},
			{"server":"config","tool":"delete_config","arguments":{"path":"admin/orders"}}]}`, nil
	})
	used := map[string]bool{}
	m.pictureLookups(ctx, planner, pictureNoteCache{pictures, p}, p, &research, used)

	if len(requested) != 1 || requested[0].Path != "admin/orders" || requested[0].Source != "consul" {
		t.Fatalf("executed calls: %+v", requested)
	}
	var found *bigpicture.ResearchItem
	for i := range research.Items {
		if research.Items[i].Evidence.Source.Kind == "mcp" {
			found = &research.Items[i]
		}
	}
	if found == nil || !strings.Contains(found.Content, "order-created") || strings.Contains(found.Content, "hunter2") {
		t.Fatalf("lookup evidence: %+v", found)
	}
	if !strings.HasPrefix(found.Evidence.Locator, "tool:get_config") {
		t.Fatalf("locator: %s", found.Evidence.Locator)
	}
	notes := strings.Join(research.Notes, "\n")
	if !strings.Contains(notes, "does not occur in the collected repository material") || !strings.Contains(notes, "not granted") {
		t.Fatalf("rejections not reported: %s", notes)
	}
	if len(used) != 1 {
		t.Fatalf("plan not cached: %v", used)
	}
}
