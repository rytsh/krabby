package manager

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestPushItemsUpsertByIDAndPrune(t *testing.T) {
	ctx := context.Background()
	m, store := newReconcileManager(t, &fakeReconcileFetcher{})
	if err := store.UpsertCollection(ctx, &websource.Collection{Name: "ci", Type: websource.TypePush}); err != nil {
		t.Fatal(err)
	}

	csv := base64.StdEncoding.EncodeToString([]byte("host,port\ndb,5432\n"))
	items := []WebPageImport{
		{ID: "runbook/db", Title: "DB runbook", ContentType: "text/markdown", Content: "restart alpha", Teams: []string{"Platform"}},
		{ID: "inventory", ContentType: "text/csv", Content: csv, Encoding: "base64"},
	}
	result, err := m.ImportWebPages(ctx, "ci", items)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 || result.Changed != 2 {
		t.Fatalf("result = %+v", result)
	}

	content, err := os.ReadFile(m.sourcesDir("ci") + "/" + PushSlug("inventory") + ".md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "| db | 5432 |") {
		t.Fatalf("csv markdown = %q", content)
	}
	page, err := store.GetPage(ctx, websource.PageID("ci", PushSlug("runbook/db")))
	if err != nil || page == nil || len(page.Teams) != 1 {
		t.Fatalf("page = %+v %v", page, err)
	}

	if _, err := m.ImportWebPages(ctx, "ci", []WebPageImport{{Title: "x", ContentType: "text/plain", Content: "y"}}); err == nil {
		t.Fatal("push without id accepted")
	}

	removed, err := m.PrunePushItems(ctx, "ci", []string{"runbook/db"})
	if err != nil || removed != 1 {
		t.Fatalf("prune = %d %v", removed, err)
	}
	pages, _ := store.Pages(ctx, "ci")
	if len(pages) != 1 || pages[0].Slug != PushSlug("runbook/db") {
		t.Fatalf("pages after prune = %+v", pages)
	}
}
