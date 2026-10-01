package manager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/storage"
)

func TestBigPictureSourceValidationAndIndependentNamespaces(t *testing.T) {
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
	web, err := websource.New(db)
	if err != nil {
		t.Fatal(err)
	}
	apis, err := apicatalog.New(db)
	if err != nil {
		t.Fatal(err)
	}
	connections, err := mcpclient.New(db)
	if err != nil {
		t.Fatal(err)
	}
	pictures, err := bigpicture.New(db, filepath.Join(dir, "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := &Manager{reg: reg, webStore: web, apiStore: apis, externalMCPs: connections, bigPictures: pictures}
	if err := reg.Upsert(ctx, &registry.Repo{ID: "example/team/service", Namespace: "backend", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := web.UpsertCollection(ctx, &websource.Collection{Name: "runbooks", Type: "pages", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := apis.UpsertService(ctx, &apicatalog.Service{Name: "payment", Kind: "openapi", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	if _, err := connections.Save(ctx, "", mcpclient.Config{Name: "config", URL: "https://config.example/mcp", BearerToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	sources := []bigpicture.Source{{Kind: "repo", Ref: "example/team/service"}, {Kind: "web", Ref: "runbooks"}, {Kind: "api", Ref: "payment"}, {Kind: "mcp", Ref: "config"}}
	cfg := bigpicture.Config{Name: "commerce", Title: "Commerce", Namespace: "commerce", Prompt: "Explain the system", Sources: sources}
	if _, err := mgr.SaveBigPicture(ctx, "", cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Name, cfg.Namespace = "incidents", "operations"
	if _, err := mgr.SaveBigPicture(ctx, "", cfg); err != nil {
		t.Fatal(err)
	}
	repo, _ := reg.Get(ctx, "example/team/service")
	if repo.Namespace != "backend" {
		t.Fatal("workspace selection reassigned a repository namespace")
	}
	connection, _ := connections.Get(ctx, "config")
	if len(connection.AllowedTools) != 0 || len(connection.AllowedResources) != 0 {
		t.Fatal("source selection widened external MCP grants")
	}
	for _, kind := range []string{"repo", "web", "api", "mcp"} {
		page, err := mgr.BigPictureSourceOptions(ctx, kind, "", 1, 20)
		if err != nil || page.Total != 1 || len(page.Items) != 1 {
			t.Fatalf("options %s: %+v %v", kind, page, err)
		}
		cfg.Name = "missing-" + kind
		cfg.Sources = []bigpicture.Source{{Kind: kind, Ref: "missing"}}
		if _, err := mgr.SaveBigPicture(ctx, "", cfg); !errors.Is(err, bigpicture.ErrInvalid) {
			t.Fatalf("missing %s source accepted", kind)
		}
	}
}
