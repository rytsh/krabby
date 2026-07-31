package manager

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

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

	oldUpdatedAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
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
			UpdatedAt:   oldUpdatedAt,
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

	newUpdatedAt := oldUpdatedAt.Add(time.Hour)
	pages[1].UpdatedAt = newUpdatedAt
	result, err = m.ImportWebPages(ctx, "offline", pages)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 || result.Changed != 0 || result.Unchanged != 2 {
		t.Fatalf("second import = %+v", result)
	}
	docs, err := m.SearchDocs(ctx, ScopeSources, websource.ScopeKey("offline"), "", DocsSearchLexical, "recovery", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || !docs[0].UpdatedAt.Equal(newUpdatedAt) {
		t.Fatalf("reindexed metadata = %#v", docs)
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

func TestWebPageURLValidation(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"", "javascript:alert(1)", "https:///missing-host", "https://user:secret@example.com/page"} {
		if _, err := validateWebPageURL(invalid); err == nil {
			t.Errorf("validateWebPageURL(%q) accepted invalid URL", invalid)
		}
	}
	if got, err := validateWebPageURL("  https://example.com/page?q=1  "); err != nil || got != "https://example.com/page?q=1" {
		t.Fatalf("valid URL = %q, err=%v", got, err)
	}
}

func TestAddWebPagePreservesExistingMetadata(t *testing.T) {
	ctx := context.Background()
	m, store := newReconcileManager(t, &fakeReconcileFetcher{})
	if err := store.UpsertCollection(ctx, &websource.Collection{Name: "pages", Type: websource.TypePages}); err != nil {
		t.Fatal(err)
	}
	pageURL := "https://example.com/runbook"
	slug := slugForURL(pageURL)
	if err := store.UpsertPage(ctx, &websource.Page{
		ID: websource.PageID("pages", slug), Collection: "pages", Slug: slug,
		URL: pageURL, Title: "Runbook", Hash: "existing-hash", Status: websource.StatusReady,
	}); err != nil {
		t.Fatal(err)
	}
	release := blockQueue(t, m.queue)
	defer release()
	page, err := m.AddWebPage(ctx, "pages", pageURL)
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "Runbook" || page.Hash != "existing-hash" || page.Status != websource.StatusPending {
		t.Fatalf("registered page = %#v", page)
	}
}

func TestAddWebPageReusesLegacyURLSlug(t *testing.T) {
	ctx := context.Background()
	m, store := newReconcileManager(t, &fakeReconcileFetcher{})
	if err := store.UpsertCollection(ctx, &websource.Collection{Name: "pages", Type: websource.TypePages}); err != nil {
		t.Fatal(err)
	}
	pageURL := "https://example.com/legacy"
	legacySlug := legacySlugForURL(pageURL)
	if err := store.UpsertPage(ctx, &websource.Page{
		ID: websource.PageID("pages", legacySlug), Collection: "pages", Slug: legacySlug,
		URL: pageURL, Title: "Legacy", Status: websource.StatusReady,
	}); err != nil {
		t.Fatal(err)
	}
	release := blockQueue(t, m.queue)
	defer release()
	page, err := m.AddWebPage(ctx, "pages", pageURL)
	if err != nil {
		t.Fatal(err)
	}
	if page.Slug != legacySlug || page.Title != "Legacy" {
		t.Fatalf("legacy page = %#v", page)
	}
}

func TestImportWebPagesRejectsOversizedContent(t *testing.T) {
	ctx := context.Background()
	m, store := newReconcileManager(t, &fakeReconcileFetcher{})
	if err := store.UpsertCollection(ctx, &websource.Collection{Name: "notes", Type: websource.TypePages}); err != nil {
		t.Fatal(err)
	}
	_, err := m.ImportWebPages(ctx, "notes", []WebPageImport{{
		Title: "Large", ContentType: "text/markdown", Content: strings.Repeat("x", maxWebPageImportBytes+1),
	}})
	if err == nil || !strings.Contains(err.Error(), "content exceeds") {
		t.Fatalf("oversized import error = %v", err)
	}
}

func TestImportWebPagesValidatesPayloadBeforeQueueing(t *testing.T) {
	t.Parallel()
	m := &Manager{}
	_, err := m.ImportWebPages(context.Background(), "notes", []WebPageImport{{
		Content: strings.Repeat("x", maxWebPageImportBytes+1),
	}})
	if err == nil || !strings.Contains(err.Error(), "content exceeds") {
		t.Fatalf("pre-queue validation error = %v", err)
	}
	_, err = m.ImportWebPages(context.Background(), "notes", []WebPageImport{{
		URL: "https://user:secret@example.com/page", ContentType: "text/plain", Content: "small",
	}})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("pre-queue URL validation error = %v", err)
	}
	_, err = m.ImportWebPages(context.Background(), "notes", []WebPageImport{{
		Title: strings.Repeat("x", maxWebPageTitleBytes+1), ContentType: "text/plain", Content: "small",
	}})
	if err == nil || !strings.Contains(err.Error(), "title exceeds") {
		t.Fatalf("pre-queue title validation error = %v", err)
	}
}
