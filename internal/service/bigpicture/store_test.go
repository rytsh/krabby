package bigpicture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/rytsh/krabby/internal/storage"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(db, filepath.Join(dir, "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testConfig(name string) Config {
	return Config{Name: name, Title: "Commerce", Namespace: " commerce ", Prompt: "Explain deployment and event flows.", Sources: []Source{{Kind: "repo", Ref: "example.com/team/checkout"}}}
}

func testPublication(version uint64, previous string) Publication {
	return Publication{ExpectedVersion: version, ExpectedRevision: previous, Producer: "test-agent", Overview: "overview.md", Documents: []Document{
		{Path: "overview.md", Title: "Overview", Markdown: "# Overview\nSee [Checkout](services/checkout.md).", Evidence: []Evidence{{Source: Source{Kind: "repo", Ref: "example.com/team/checkout"}, Locator: "cmd/main.go", Revision: "abc123"}}},
		{Path: "services/checkout.md", Title: "Checkout", Markdown: "# Checkout\n[Back](../overview.md)"},
	}}
}

func TestWorkspaceLifecycleAndNamespace(t *testing.T) {
	s, ctx := newTestStore(t), context.Background()
	cfg := testConfig("commerce")
	p, err := s.Save(ctx, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || p.Namespace != "commerce" || len(p.Revisions) != 0 {
		t.Fatalf("workspace: %+v", p)
	}
	if _, err := s.Save(ctx, "", cfg); !errors.Is(err, ErrExists) {
		t.Fatal("duplicate name accepted")
	}
	if _, err := s.Save(ctx, "commerce", cfg); !errors.Is(err, ErrConflict) {
		t.Fatal("stale config update accepted")
	}
	cfg.ExpectedVersion, cfg.Description = 1, "Production system"
	p, err = s.Save(ctx, "commerce", cfg)
	if err != nil || p.Version != 2 {
		t.Fatalf("update failed: %v", err)
	}
	other := testConfig("default-workspace")
	other.Namespace = ""
	if _, err := s.Save(ctx, "", other); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		opts  ListOptions
		count int
	}{
		{ListOptions{}, 1}, {ListOptions{Namespace: "commerce"}, 1}, {ListOptions{Namespace: "*"}, 2},
		{ListOptions{Namespace: "*", Query: "Production"}, 1}, {ListOptions{Namespace: "*", Page: 1000000000}, 0},
	} {
		page, err := s.List(ctx, tc.opts)
		if err != nil || len(page.Items) != tc.count {
			t.Fatalf("list %+v: %+v, %v", tc.opts, page, err)
		}
	}
	if err := s.Delete(ctx, p.Name, 1); !errors.Is(err, ErrConflict) {
		t.Fatal("stale deletion accepted")
	}
	if err := s.Delete(ctx, p.Name, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, p.Name); !errors.Is(err, ErrNotFound) {
		t.Fatal("workspace survived deletion")
	}
}

func TestPublicationIsImmutableAndGuarded(t *testing.T) {
	s, ctx := newTestStore(t), context.Background()
	cfg := testConfig("commerce")
	p, err := s.Save(ctx, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Publish(ctx, p.Name, testPublication(1, ""))
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfigVersion != 1 || len(first.Documents) != 2 || len(first.Documents[0].Hash) != 64 {
		t.Fatalf("manifest: %+v", first)
	}
	doc, err := s.ReadDocument(ctx, p.Name, first.ID, "overview.md", 0, 0)
	if err != nil || doc.Title != "Overview" || len(doc.Evidence) != 1 {
		t.Fatalf("read: %+v, %v", doc, err)
	}
	if _, err := s.Publish(ctx, p.Name, testPublication(1, "")); !errors.Is(err, ErrConflict) {
		t.Fatal("stale publisher overwrote current revision")
	}
	cfg.ExpectedVersion, cfg.Prompt = 1, "A new scope"
	if _, err := s.Save(ctx, p.Name, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, p.Name, testPublication(1, first.ID)); !errors.Is(err, ErrConflict) {
		t.Fatal("old config publication accepted")
	}
	second, err := s.Publish(ctx, p.Name, testPublication(2, first.ID))
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.Snapshot(ctx, p.Name, first.ID)
	if err != nil || old.Prompt == cfg.Prompt {
		t.Fatal("historical config was mutated")
	}
	current, err := s.Snapshot(ctx, p.Name, "")
	if err != nil || current.ID != second.ID {
		t.Fatal("current pointer not updated")
	}
	bad := testPublication(2, second.ID)
	bad.Documents[1].Path = "../../escape.md"
	if _, err := s.Publish(ctx, p.Name, bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("traversal accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Publish(cancelled, p.Name, testPublication(2, second.ID)); err == nil {
		t.Fatal("cancelled publication succeeded")
	}
	current, _ = s.Snapshot(ctx, p.Name, "")
	if current.ID != second.ID {
		t.Fatal("failed publication changed current")
	}
	if _, err := s.ReadDocument(ctx, p.Name, "", "../../escape.md", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("unlisted document readable")
	}
}

func TestPublicationRetentionAndNameReuse(t *testing.T) {
	s, ctx := newTestStore(t), context.Background()
	p, err := s.Save(ctx, "", testConfig("commerce"))
	if err != nil {
		t.Fatal(err)
	}
	var first, current string
	for i := range MaxRevisions + 1 {
		snapshot, err := s.Publish(ctx, p.Name, testPublication(1, current))
		if err != nil {
			t.Fatal(err)
		}
		current = snapshot.ID
		if i == 0 {
			first = current
		}
	}
	p, _ = s.Get(ctx, p.Name)
	if len(p.Revisions) != MaxRevisions {
		t.Fatal("history not bounded")
	}
	if _, err := s.Snapshot(ctx, p.Name, first); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired revision accessible")
	}
	if _, err := os.Stat(filepath.Join(s.root, p.StorageID, first)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired files not cleaned")
	}
	if err := s.Delete(ctx, p.Name, p.Version); err != nil {
		t.Fatal(err)
	}
	recreated, err := s.Save(ctx, "", testConfig("commerce"))
	if err != nil {
		t.Fatal(err)
	}
	if recreated.StorageID == p.StorageID {
		t.Fatal("reused old filesystem identity")
	}
	if _, err := s.Snapshot(ctx, recreated.Name, current); !errors.Is(err, ErrNotFound) {
		t.Fatal("recreated workspace exposed old revision")
	}
}

func TestConcurrentPublicationHasOneWinner(t *testing.T) {
	s, ctx := newTestStore(t), context.Background()
	if _, err := s.Save(ctx, "", testConfig("commerce")); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_, err := s.Publish(ctx, "commerce", testPublication(1, ""))
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("unexpected publish error: %v", err)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("publication compare-and-swap failed")
	}
}

func TestUnicodeDocumentPagination(t *testing.T) {
	s, ctx := newTestStore(t), context.Background()
	if _, err := s.Save(ctx, "", testConfig("commerce")); err != nil {
		t.Fatal(err)
	}
	pub := testPublication(1, "")
	pub.Documents[0].Markdown = "🙂 Türkçe mimari: ödeme akışı."
	snapshot, err := s.Publish(ctx, "commerce", pub)
	if err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	var offset int64
	for {
		doc, err := s.ReadDocument(ctx, "commerce", snapshot.ID, "overview.md", offset, 5)
		if err != nil || !utf8.ValidString(doc.Content) {
			t.Fatalf("invalid page: %v", err)
		}
		content.WriteString(doc.Content)
		offset += int64(doc.Bytes)
		if !doc.Truncated {
			break
		}
		if doc.Bytes == 0 {
			t.Fatal("pagination made no progress")
		}
	}
	if content.String() != pub.Documents[0].Markdown {
		t.Fatal("UTF-8 text changed during pagination")
	}
	if _, err := s.ReadDocument(ctx, "commerce", snapshot.ID, "overview.md", 1, 5); !errors.Is(err, ErrInvalid) {
		t.Fatal("split rune offset accepted")
	}
}

func TestPublicationValidation(t *testing.T) {
	p := &Picture{Version: 1, Sources: testConfig("commerce").Sources}
	for _, value := range []string{"../escape.md", "/abs.md", "a\\b.md", "./a.md", "a//b.md", ".hidden.md", "a.txt", "a/../../b.md"} {
		if ValidDocumentPath(value) {
			t.Errorf("unsafe path %q accepted", value)
		}
	}
	for _, mutate := range []func(*Publication){
		func(pub *Publication) { pub.Overview = "missing.md" },
		func(pub *Publication) { pub.Documents = append(pub.Documents, pub.Documents[0]) },
		func(pub *Publication) { pub.Documents[0].Evidence[0].Source.Ref = "unselected" },
		func(pub *Publication) { pub.Documents[0].Markdown = strings.Repeat("x", MaxDocumentBytes+1) },
		func(pub *Publication) { pub.Documents[1].Path = "overview.md/child.md" },
	} {
		pub := testPublication(1, "")
		mutate(&pub)
		if err := ValidatePublication(p, pub); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid publication accepted: %v", err)
		}
	}
}

func TestFailedFilesystemWriteKeepsCurrentPublication(t *testing.T) {
	s, ctx := newTestStore(t), context.Background()
	p, err := s.Save(ctx, "", testConfig("commerce"))
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.root, p.StorageID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, p.Name, testPublication(1, "")); err == nil {
		t.Fatal("symlink escape permitted")
	}
	current, err := s.Get(ctx, p.Name)
	if err != nil || current.CurrentRevision != "" {
		t.Fatal("failed write published a pointer")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("publication wrote outside root")
	}
}

func TestPublicationSurvivesReopeningState(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(dir, "pictures")
	s, err := New(db, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "", testConfig("commerce")); err != nil {
		t.Fatal(err)
	}
	first, err := s.Publish(ctx, "commerce", testPublication(1, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = storage.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	s, err = New(db, root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Snapshot(ctx, "commerce", "")
	if err != nil || snapshot.ID != first.ID {
		t.Fatalf("reopened snapshot: %+v %v", snapshot, err)
	}
	doc, err := s.ReadDocument(ctx, "commerce", first.ID, "services/checkout.md", 0, 0)
	if err != nil || doc.Content != "# Checkout\n[Back](../overview.md)" {
		t.Fatalf("reopened document: %+v %v", doc, err)
	}
}
