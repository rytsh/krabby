package manager

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestImportWebPagesWritesAndIndexesClientContent(t *testing.T) {
	ctx := context.Background()
	m, store := newReconcileManager(t, &fakeReconcileFetcher{})
	if err := store.UpsertCollection(ctx, &websource.Collection{
		Name: "offline", Type: websource.TypePages, Status: websource.StatusPending,
	}); err != nil {
		t.Fatal(err)
	}

	pages := []WebPageImport{
		{
			URL:         "https://example.com/guide",
			ContentType: "text/html; charset=utf-8",
			Content:     `<html><head><title>Guide</title></head><body><main><h1>Guide</h1><p>alpha offline content</p></main></body></html>`,
		},
		{
			URL:         "https://example.com/runbook",
			Title:       "Runbook",
			ContentType: "text/markdown",
			Content:     "beta recovery steps",
		},
	}

	result, err := m.ImportWebPages(ctx, "offline", pages)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 || result.Changed != 2 || result.Unchanged != 0 {
		t.Fatalf("first import = %+v", result)
	}
	snapshot := m.TaskSnapshot()
	if len(snapshot.Tasks) == 0 || snapshot.Tasks[0].Kind != taskKindWebImport || snapshot.Tasks[0].State != "done" {
		t.Fatalf("import task history = %#v", snapshot.Tasks)
	}

	slug := slugForURL(pages[1].URL)
	content, err := os.ReadFile(m.sourcesDir("offline") + "/" + slug + ".md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "# Runbook\n\nbeta recovery steps") {
		t.Fatalf("markdown = %q", content)
	}

	indexed, err := m.docs.rag.IndexedPaths(ctx, websource.ScopeKey("offline"))
	if err != nil {
		t.Fatal(err)
	}
	if len(indexed) != 2 {
		t.Fatalf("indexed paths = %#v", indexed)
	}

	result, err = m.ImportWebPages(ctx, "offline", pages)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 || result.Changed != 0 || result.Unchanged != 2 {
		t.Fatalf("second import = %+v", result)
	}
}

func TestImportWebPagesRejectsUnsupportedContent(t *testing.T) {
	ctx := context.Background()
	m, store := newReconcileManager(t, &fakeReconcileFetcher{})
	if err := store.UpsertCollection(ctx, &websource.Collection{
		Name: "offline", Type: websource.TypePages,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := m.ImportWebPages(ctx, "offline", []WebPageImport{{
		URL: "https://example.com/data", ContentType: "application/json", Content: `{}`,
	}})
	if err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("error = %v", err)
	}
}

func TestImportWebPagesAcceptsManuallyAuthoredMarkdown(t *testing.T) {
	ctx := context.Background()
	m, store := newReconcileManager(t, &fakeReconcileFetcher{})
	if err := store.UpsertCollection(ctx, &websource.Collection{
		Name: "notes", Type: websource.TypePages,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := m.ImportWebPages(ctx, "notes", []WebPageImport{{
		Title: "Recovery Notes", ContentType: "text/markdown", Content: "Restart the worker.",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 || result.Changed != 1 {
		t.Fatalf("result = %+v", result)
	}

	slug := "manual-recovery-notes-" + websource.Hash("Recovery Notes")[:8]
	page, err := store.GetPage(ctx, websource.PageID("notes", slug))
	if err != nil {
		t.Fatal(err)
	}
	if page == nil || page.URL != "" || page.Title != "Recovery Notes" || page.Status != websource.StatusReady {
		t.Fatalf("page = %+v", page)
	}
	content, err := os.ReadFile(m.sourcesDir("notes") + "/" + slug + ".md")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != "# Recovery Notes\n\nRestart the worker.\n" {
		t.Fatalf("markdown = %q", got)
	}
}

func TestImportWebPagesRequiresTitleWithoutURL(t *testing.T) {
	ctx := context.Background()
	m, store := newReconcileManager(t, &fakeReconcileFetcher{})
	if err := store.UpsertCollection(ctx, &websource.Collection{
		Name: "notes", Type: websource.TypePages,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := m.ImportWebPages(ctx, "notes", []WebPageImport{{
		ContentType: "text/markdown", Content: "Untitled",
	}})
	if err == nil || !strings.Contains(err.Error(), "title is required") {
		t.Fatalf("error = %v", err)
	}
}
