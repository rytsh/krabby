package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/storage"
)

func TestBigPictureRepoPatternResearch(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("Service readme."), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"github.com/acme/orders", "github.com/acme/infra/deploy", "github.com/other/orders", "gitlab.com/acme/orders"} {
		if err := reg.Upsert(ctx, &registry.Repo{ID: id, Path: root, Status: "ready"}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := m.BigPictureSourceOptions(ctx, "repo_pattern", "https://github.com/acme/", 1, 20)
	if err != nil || page.Total != 1 || page.Items[0].Ref != "github.com/acme/**" || page.Items[0].Status != "2 repositories" {
		t.Fatalf("pattern preview: %+v %v", page, err)
	}

	selector := bigpicture.Source{Kind: "repo_pattern", Ref: "github.com/acme/**"}
	p, err := m.SaveBigPicture(ctx, "", bigpicture.Config{Name: "acme", Title: "Acme", Prompt: "Explain", Sources: []bigpicture.Source{selector}})
	if err != nil {
		t.Fatal(err)
	}
	// Repositories added after saving are picked up on the next run.
	if err := reg.Upsert(ctx, &registry.Repo{ID: "github.com/Acme/billing", Path: root}); err != nil {
		t.Fatal(err)
	}
	research, err := m.collectPictureResearch(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	repos := map[string]bool{}
	for _, item := range research.Items {
		if item.Evidence.Source != selector {
			t.Fatalf("evidence not attributed to the pattern: %+v", item.Evidence)
		}
		repos[strings.SplitN(item.Evidence.Locator, ":", 2)[0]] = true
	}
	if len(repos) != 3 || !repos["github.com/acme/orders"] || !repos["github.com/acme/infra/deploy"] || !repos["github.com/Acme/billing"] {
		t.Fatalf("pattern resolved to %v", repos)
	}

	for _, bad := range []string{"", "github.com/a**", "github.com/../x", "github.com/[a"} {
		if _, err := bigpicture.NormalizeRepoPattern(bad); err == nil {
			t.Fatalf("invalid pattern %q accepted", bad)
		}
	}
}

func TestBigPictureSkipsUnrelatedSourceContent(t *testing.T) {
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
	web, err := websource.New(db)
	if err != nil {
		t.Fatal(err)
	}
	text, err := rag.NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(dir, "sources")
	pages := map[string]map[string]string{
		"wiki": {
			"checkout-kafka.md": "# Checkout\nCheckout publishes order-created to Kafka.",
			"holiday-party.md":  "# Party\nThe holiday party is on Friday.",
		},
		"hr": {
			"vacation.md": "# Vacation\nRequest vacation days in the portal.",
		},
	}
	for name, files := range pages {
		if err := os.MkdirAll(filepath.Join(sources, name), 0700); err != nil {
			t.Fatal(err)
		}
		for file, body := range files {
			if err := os.WriteFile(filepath.Join(sources, name, file), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := web.UpsertCollection(ctx, &websource.Collection{Name: name, Type: "pages"}); err != nil {
			t.Fatal(err)
		}
		if err := text.Index(ctx, websource.ScopeKey(name), filepath.Join(sources, name)); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{bigPictures: store, webStore: web, docsText: text, sourcesRootDir: sources}
	p := &bigpicture.Picture{Name: "commerce", Title: "Commerce", Prompt: "Explain checkout Kafka event flows", Sources: []bigpicture.Source{{Kind: "web", Ref: "hr"}, {Kind: "web", Ref: "wiki"}}}
	research, err := m.collectPictureResearch(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(research.Items) != 1 || research.Items[0].Evidence.Locator != "checkout-kafka.md" {
		t.Fatalf("expected only the related page, got %+v", research.Items)
	}
	if !strings.Contains(strings.Join(research.Notes, "\n"), "web:hr: no indexed document matched") {
		t.Fatalf("unrelated source not reported: %v", research.Notes)
	}
}
