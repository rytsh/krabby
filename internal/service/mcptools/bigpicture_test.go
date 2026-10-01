package mcptools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/storage"
)

func TestBigPictureMCPPublishAndRead(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	connections, err := mcpclient.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connections.Save(ctx, "", mcpclient.Config{Name: "config", URL: "https://example.com/mcp"}); err != nil {
		t.Fatal(err)
	}
	pictures, err := bigpicture.New(db, filepath.Join(dir, "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := &manager.Manager{}
	mgr.SetBigPictures(pictures)
	mgr.SetExternalMCPs(connections)
	connect := func(server *mcp.Server) *mcp.ClientSession {
		t.Helper()
		ct, st := mcp.NewInMemoryTransports()
		if _, err := server.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
		session, err := client.Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	core, admin := connect(NewCore(mgr, "test")), connect(NewAdmin(mgr, "test", 0))
	cfg := bigpicture.Config{Name: "commerce", Title: "Commerce", Namespace: "commerce", Prompt: "Explain the system", Sources: []bigpicture.Source{{Kind: "mcp", Ref: "config"}}}
	data, _ := json.Marshal(cfg)
	text, failed := callToolText(t, admin, "save_big_picture", map[string]any{"config": string(data)})
	if failed || !strings.Contains(text, "commerce") {
		t.Fatalf("save: %s", text)
	}
	text, failed = callToolText(t, core, "get_big_picture", map[string]any{"name": "commerce"})
	if failed || !strings.Contains(text, "No documents published") {
		t.Fatalf("unpublished inspect: %s", text)
	}
	pub := bigpicture.Publication{ExpectedVersion: 1, Producer: "test-agent", Overview: "overview.md", Documents: []bigpicture.Document{{Path: "overview.md", Title: "Overview", Markdown: "# Architecture"}}}
	data, _ = json.Marshal(pub)
	text, failed = callToolText(t, admin, "publish_big_picture", map[string]any{"name": "commerce", "publication": string(data)})
	if failed {
		t.Fatalf("publish: %s", text)
	}
	text, failed = callToolText(t, core, "get_big_picture", map[string]any{"name": "commerce", "path": "overview.md"})
	if failed || !strings.Contains(text, "# Architecture") {
		t.Fatalf("read: %s", text)
	}
	text, failed = callToolText(t, core, "list_big_pictures", map[string]any{"namespace": "commerce"})
	if failed || !strings.Contains(text, "commerce") {
		t.Fatalf("list: %s", text)
	}
	_, failed = callToolText(t, admin, "publish_big_picture", map[string]any{"name": "commerce", "publication": string(data)})
	if !failed {
		t.Fatal("stale publication accepted through MCP")
	}
}
