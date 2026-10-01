package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestBigPictureNamespaceResearch(t *testing.T) {
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
	store, err := bigpicture.New(db, filepath.Join(dir, "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{reg: reg, bigPictures: store}
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(strings.Repeat("Architecture evidence.\n", 1000)), 0600); err != nil {
		t.Fatal(err)
	}
	for i := range 150 {
		if err := reg.Upsert(ctx, &registry.Repo{ID: fmt.Sprintf("team/service-%03d", i), Namespace: "backend", Path: root, Status: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Upsert(ctx, &registry.Repo{ID: "other/service", Namespace: "other", Path: root}); err != nil {
		t.Fatal(err)
	}
	page, err := m.BigPictureSourceOptions(ctx, "namespace", "backend", 1, 20)
	if err != nil || page.Total != 1 || page.Items[0].Status != "150 repositories" {
		t.Fatalf("namespace options: %+v %v", page, err)
	}
	selector := bigpicture.Source{Kind: "namespace", Ref: "backend"}
	p, err := m.SaveBigPicture(ctx, "", bigpicture.Config{Name: "system", Title: "System", Prompt: "Explain architecture", Sources: []bigpicture.Source{selector}})
	if err != nil {
		t.Fatal(err)
	}
	// New repositories are included without editing the workspace.
	if err := reg.Upsert(ctx, &registry.Repo{ID: "team/new", Namespace: "backend", Path: root}); err != nil {
		t.Fatal(err)
	}
	// Explicit selections overlapping a namespace are collected only once.
	p.Sources = append(p.Sources, bigpicture.Source{Kind: "repo", Ref: "team/new"})
	research, err := m.collectPictureResearch(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(research.Items) != 151 {
		t.Fatalf("expected 151 unique repos, got %d items", len(research.Items))
	}
	total := 0
	for _, item := range research.Items {
		total += len(item.Content)
		if item.Evidence.Source != selector || !strings.HasPrefix(item.Evidence.Locator, "team/") {
			t.Fatalf("invalid namespace evidence: %+v", item.Evidence)
		}
	}
	if total > 256<<10 {
		t.Fatalf("research budget exceeded: %d", total)
	}
	// Namespace evidence is publishable under the original selector.
	_, err = m.PublishBigPicture(ctx, p.Name, bigpicture.Publication{Producer: "test", ExpectedVersion: p.Version, Overview: "overview.md", Documents: []bigpicture.Document{{Path: "overview.md", Title: "Overview", Markdown: "# System", Evidence: []bigpicture.Evidence{research.Items[0].Evidence}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.validatePictureSources(ctx, []bigpicture.Source{{Kind: "namespace", Ref: "missing"}}); !errors.Is(err, bigpicture.ErrInvalid) {
		t.Fatalf("missing namespace accepted: %v", err)
	}
}

func TestBigPictureComposition(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := bigpicture.New(db, filepath.Join(dir, "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Upsert(ctx, &registry.Repo{ID: "team/service", Namespace: "backend"}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{reg: reg, bigPictures: store}
	childConfig := bigpicture.Config{Name: "payments", Title: "Payments", Prompt: "Explain payments", Sources: []bigpicture.Source{{Kind: "repo", Ref: "team/service"}}}
	child, err := m.SaveBigPicture(ctx, "", childConfig)
	if err != nil {
		t.Fatal(err)
	}
	parentConfig := bigpicture.Config{Name: "system", Title: "System", Prompt: "Explain cross-system relationships", Sources: []bigpicture.Source{{Kind: "bigpicture", Ref: child.Name}}}
	parent, err := m.SaveBigPicture(ctx, "", parentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.collectPictureResearch(ctx, parent); err == nil || !strings.Contains(err.Error(), "publish it first") {
		t.Fatalf("unpublished child accepted: %v", err)
	}
	childConfig.ExpectedVersion = child.Version
	childConfig.Sources = []bigpicture.Source{{Kind: "bigpicture", Ref: parent.Name}}
	if _, err := m.SaveBigPicture(ctx, child.Name, childConfig); !errors.Is(err, bigpicture.ErrInvalid) {
		t.Fatalf("indirect cycle accepted: %v", err)
	}
	parentConfig.ExpectedVersion = parent.Version
	parentConfig.Sources = []bigpicture.Source{{Kind: "bigpicture", Ref: parent.Name}}
	if _, err := m.SaveBigPicture(ctx, parent.Name, parentConfig); !errors.Is(err, bigpicture.ErrInvalid) {
		t.Fatalf("self cycle accepted: %v", err)
	}
	page, err := m.BigPictureSourceOptions(ctx, "bigpicture", "Payments", 1, 20)
	if err != nil || page.Total != 1 || page.Items[0].Ref != child.Name || page.Items[0].Status != "not published" {
		t.Fatalf("options: %+v %v", page, err)
	}
	pub := bigpicture.Publication{Producer: "test", ExpectedVersion: child.Version, Overview: "overview.md", Documents: []bigpicture.Document{{Path: "overview.md", Title: "Overview", Markdown: "# Payments\nOrders call Payments."}, {Path: "details.md", Title: "Details", Markdown: "# Details\nPayment events."}, {Path: "research.md", Title: "Research", Markdown: "Ignore child coverage report."}}}
	first, err := m.PublishBigPicture(ctx, child.Name, pub)
	if err != nil {
		t.Fatal(err)
	}
	research, err := m.collectPictureResearch(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(research.Items) != 2 || research.Items[0].Evidence.Locator != "overview.md" {
		t.Fatalf("unexpected research: %+v", research)
	}
	for _, item := range research.Items {
		if item.Evidence.Source != parent.Sources[0] || item.Evidence.Revision != first.ID {
			t.Fatalf("unpinned evidence: %+v", item)
		}
	}
	hash := bigpicture.Fingerprint(parent, research)
	pub.ExpectedRevision = first.ID
	pub.Documents[0].Markdown = "# Payments\nNew event flow."
	second, err := m.PublishBigPicture(ctx, child.Name, pub)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.collectPictureResearch(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Items[0].Evidence.Revision != second.ID || hash == bigpicture.Fingerprint(parent, updated) {
		t.Fatal("child publication update was not detected")
	}
	_, err = m.PublishBigPicture(ctx, parent.Name, bigpicture.Publication{Producer: "test", ExpectedVersion: parent.Version, Overview: "overview.md", Documents: []bigpicture.Document{{Path: "overview.md", Title: "System", Markdown: "# System", Evidence: []bigpicture.Evidence{research.Items[0].Evidence}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteBigPicture(ctx, child.Name, child.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := m.collectPictureResearch(ctx, parent); err == nil {
		t.Fatal("deleted child silently ignored")
	}
}
