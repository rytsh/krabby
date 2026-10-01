package manager

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/storage"
)

func TestBigPictureSearchModesNamespaceAndPublicationIsolation(t *testing.T) {
	ctx := context.Background()
	f := newHybridFixture(t, 0, 0)
	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pictures, err := bigpicture.New(db, filepath.Join(t.TempDir(), "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	f.m.SetBigPictures(pictures)
	create := func(name, namespace string) *bigpicture.Snapshot {
		t.Helper()
		_, err := pictures.Save(ctx, "", bigpicture.Config{Name: name, Title: name, Namespace: namespace, Prompt: "Explain flows", Sources: []bigpicture.Source{{Kind: "repo", Ref: "service"}}})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.m.PublishBigPicture(ctx, name, bigpicture.Publication{ExpectedVersion: 1, Producer: "test", Overview: "overview.md", Documents: []bigpicture.Document{{Path: "overview.md", Title: "Gateway", Markdown: "# Gateway timeout\nPayment gateway timeout capture retry architecture."}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.m.indexBigPicture(ctx, name, true); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	first := create("commerce", "commerce")
	create("operations", "operations")
	for _, mode := range []string{DocsSearchLexical, DocsSearchSemantic, DocsSearchHybrid} {
		page, err := f.m.SearchDocs(ctx, ScopeBigPictures, "", "commerce", mode, "gateway timeout", 10)
		if err != nil || len(page.Results) != 1 {
			t.Fatalf("%s namespace search: %+v %v", mode, page, err)
		}
		hit := page.Results[0]
		if hit.Repo != "bigpicture:commerce" || hit.SourceKind != "bigpicture" || hit.Namespace != "commerce" || hit.Revision != first.ID {
			t.Fatalf("wrong metadata: %+v", hit)
		}
		page, err = f.m.SearchDocs(ctx, ScopeAll, "", "commerce", mode, "gateway timeout", 10)
		if err != nil || len(page.Results) != 1 || page.Results[0].Namespace != "commerce" {
			t.Fatalf("BP-only namespace in all scope: %+v %v", page, err)
		}
	}
	second, err := f.m.PublishBigPicture(ctx, "commerce", bigpicture.Publication{ExpectedVersion: 1, ExpectedRevision: first.ID, Producer: "test", Overview: "overview.md", Documents: []bigpicture.Document{{Path: "overview.md", Title: "New gateway", Markdown: "# Gateway timeout\nNew gateway timeout architecture."}}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.m.SearchDocs(ctx, ScopeBigPictures, "bigpicture:commerce", "*", DocsSearchLexical, "gateway", 10)
	if err != nil || len(page.Results) != 0 {
		t.Fatal("old publication remained searchable while current index pending")
	}
	// Wildcard broad queries exclude retired keys BEFORE ranking as well.
	page, err = f.m.SearchDocs(ctx, ScopeAll, "", "*", DocsSearchLexical, "gateway", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range page.Results {
		if hit.Repo == "bigpicture:commerce" {
			t.Fatal("wildcard leaked retired publication")
		}
	}
	if err := f.m.indexBigPicture(ctx, "commerce", false); err != nil {
		t.Fatal(err)
	}
	page, err = f.m.SearchDocs(ctx, ScopeBigPictures, "bigpicture:commerce", "operations", DocsSearchLexical, "gateway", 10)
	if err != nil || len(page.Results) != 1 || page.Results[0].Revision != second.ID {
		t.Fatal("explicit public scope not mapped to ready current index")
	}
	page, err = f.m.SearchDocs(ctx, ScopeBigPictures, "bigpicture:commerce", "*", DocsSearchSemantic, "gateway", 10)
	if err != nil || len(page.Results) != 0 {
		t.Fatal("incomplete semantic index exposed")
	}
	doc, err := f.m.GetDocDetails(ctx, bigpicture.IndexKey("commerce", first.ID), "overview.md", 0, 0)
	if err != nil || doc.Revision != first.ID || doc.ScopeKey != "bigpicture:commerce" {
		t.Fatalf("pinned old document read: %+v %v", doc, err)
	}
	// Rebuilding the current publication (for example after an embedder
	// change) keeps the live index searchable until the new one is ready.
	operations, err := pictures.Get(ctx, "operations")
	if err != nil {
		t.Fatal(err)
	}
	pending := bigpicture.IndexKey("operations", operations.CurrentRevision) + ":ZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	if _, err := pictures.BeginIndex(ctx, "operations", operations.StorageID, operations.CurrentRevision, pending); err != nil {
		t.Fatal(err)
	}
	page, err = f.m.SearchDocs(ctx, ScopeBigPictures, "bigpicture:operations", "*", DocsSearchHybrid, "gateway", 10)
	if err != nil || len(page.Results) != 1 {
		t.Fatalf("live index hidden during rebuild: %+v %v", page, err)
	}

	// A text-only rebuild leaves lexical and semantic on different attempts;
	// hybrid still fuses one document per path instead of disappearing.
	if err := f.m.indexBigPicture(ctx, "operations", false); err != nil {
		t.Fatal(err)
	}
	operations, _ = pictures.Get(ctx, "operations")
	if operations.TextIndex == operations.VectorIndex || len(operations.IndexPending) != 0 {
		t.Fatalf("expected split live keys and reclaimed attempt: %+v", operations)
	}
	page, err = f.m.SearchDocs(ctx, ScopeBigPictures, "bigpicture:operations", "*", DocsSearchHybrid, "gateway", 10)
	if err != nil || len(page.Results) != 1 || page.Results[0].Repo != "bigpicture:operations" || page.Results[0].Revision != operations.CurrentRevision {
		t.Fatalf("hybrid with split keys: %+v %v", page, err)
	}
	// The interrupted attempt's key holds no searchable data.
	if hits, err := f.m.docsText.Search(ctx, vectorstore.FilterKey(pending), "gateway", 10); err != nil || len(hits) != 0 {
		t.Fatalf("interrupted attempt not purged: %v %v", hits, err)
	}

	commerce, err := pictures.Get(ctx, "commerce")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.DeleteBigPicture(ctx, "commerce", 1); err != nil {
		t.Fatal(err)
	}
	page, err = f.m.SearchDocs(ctx, ScopeBigPictures, "", "*", DocsSearchLexical, "gateway", 10)
	if err != nil || len(page.Results) != 1 || page.Results[0].Repo != "bigpicture:operations" {
		t.Fatal("deleted workspace index leaked")
	}
	// Deletion removes every recorded index key, not only revision-derived ones.
	for _, key := range []string{commerce.TextIndex, commerce.VectorIndex} {
		if hits, err := f.m.docsText.Search(ctx, vectorstore.FilterKey(key), "gateway", 10); err != nil || len(hits) != 0 {
			t.Fatalf("deleted index data remains for %s: %v %v", key, hits, err)
		}
	}
}
