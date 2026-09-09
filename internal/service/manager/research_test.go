package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/websource"
)

func TestOverviewSectionsRespectMarkdownAndByteOffsets(t *testing.T) {
	text := "# Giriş\r\n\r\n````go\r\n## Hidden\r\n```\r\n## Also hidden\r\n````\r\n  ## Real ###\r\n~~~text\r\n# Hidden again\r\n~~~\r\n    # Indented code\r\n#not-heading\r\n## C#\r\n### Üç\r\n"
	sections, truncated := overviewSections(text, 30)
	var titles []string
	for _, section := range sections {
		titles = append(titles, section.Title)
	}
	if truncated || !reflect.DeepEqual(titles, []string{"Giriş", "Real", "C#", "Üç"}) {
		t.Fatalf("sections=%+v truncated=%t", sections, truncated)
	}
	for i, marker := range []string{"# Giriş", "  ## Real", "## C#", "### Üç"} {
		if sections[i].Offset != int64(strings.Index(text, marker)) {
			t.Fatalf("wrong byte offset for %s", marker)
		}
	}
	limited, truncated := overviewSections(text, 2)
	if len(limited) != 2 || !truncated {
		t.Fatalf("limited=%+v truncated=%t", limited, truncated)
	}
}

func TestRepoOverviewNavigatesExistingGeneratedDocument(t *testing.T) {
	m, reg := newNamespaceManager(t)
	ctx := context.Background()
	clone := t.TempDir()
	if err := os.Mkdir(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := &registry.Repo{ID: "host/team/payments", URL: "https://host/team/payments.git", Path: clone, LastCommit: "new",
		Stages: registry.Stages{Docs: registry.StageState{Status: registry.StageOK, Commit: "old"}}}
	if err := reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}
	unavailable, err := m.RepoOverview(ctx, repo.ID)
	if err != nil || unavailable.Available || !strings.Contains(unavailable.Note, "search_code") {
		t.Fatalf("unavailable=%+v err=%v", unavailable, err)
	}
	m.docsRootDir = t.TempDir()
	dir, err := m.docsDirForRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# Payments\r\nHandles capture.\r\n## Architecture\r\n" + strings.Repeat("ö", 3000) + "\r\n```go\r\n## Not a section\r\n```\r\n## Retry policy\r\nSee `internal/retry.go`.\r\n"
	if err := os.WriteFile(filepath.Join(dir, docgen.DocName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	manifest := docgen.Manifest{Repo: repo.ID, Generated: generated.Add(time.Hour), Docs: []docgen.DocMeta{{Path: docgen.DocName, Generated: generated}}, Summaries: []docgen.DocMeta{{SourcePath: "internal/retry.go"}}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, docgen.ManifestName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := m.RepoOverview(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Available || !out.Stale || out.DocsCommit != "old" || out.CloneCommit != "new" || out.EvidenceKind != "generated_summary" || !out.GeneratedAt.Equal(generated) || out.SummarizedFiles != 1 {
		t.Fatalf("overview=%+v", out)
	}
	if !out.Truncated || len(out.Overview) > 4096 || !utf8.ValidString(out.Overview) || out.NextOffset != int64(len(out.Overview)) || !strings.HasPrefix(content, out.Overview) {
		t.Fatalf("bad preview: bytes=%d next=%d", len(out.Overview), out.NextOffset)
	}
	if len(out.Sections) != 3 || out.SectionsTruncated {
		t.Fatalf("outline=%+v", out.Sections)
	}
	read, err := m.GetDocDetails(ctx, repo.ID, out.Path, out.Sections[2].Offset, 128)
	if err != nil || !strings.HasPrefix(read.Content, "## Retry policy") || read.Evidence.Kind != "generated_summary" {
		t.Fatalf("section read=%+v err=%v", read, err)
	}
	if _, err := m.RepoOverview(ctx, "unknown"); err == nil {
		t.Fatal("unknown repo accepted")
	}
	large := strings.Repeat("## Topic\nDetails.\n", 40) + strings.Repeat("x", 128<<10) + "\n## Beyond scan\n"
	if err := os.WriteFile(filepath.Join(dir, docgen.DocName), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = m.RepoOverview(ctx, repo.ID)
	if err != nil || len(out.Sections) != 30 || !out.SectionsTruncated || len(out.Overview) > 4096 {
		t.Fatalf("large overview not bounded: sections=%d err=%v", len(out.Sections), err)
	}
	// A per-file summary with a synthesis-like filename is not a repo overview.
	manifest.Docs[0].SourcePath = "internal/payments.go"
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, docgen.ManifestName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = m.RepoOverview(ctx, repo.ID)
	if err != nil || out.Available {
		t.Fatalf("per-file summary advertised as repo overview: available=%t err=%v", out.Available, err)
	}
	manifest.Docs[0].SourcePath = ""
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, docgen.ManifestName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, docgen.DocName)); err != nil {
		t.Fatal(err)
	}
	out, err = m.RepoOverview(ctx, repo.ID)
	if err != nil || out.Available {
		t.Fatalf("missing artifact=%+v err=%v", out, err)
	}
}

func TestSyncedEvidenceAndReadsAreNotLiveProviderQueries(t *testing.T) {
	m, _ := newNamespaceManager(t)
	ctx := context.Background()
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer upstream.Close()
	refreshed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	col := &websource.Collection{Name: "support", Type: websource.TypeJira, Status: websource.StatusReady, LastRefreshAt: refreshed}
	if err := m.webStore.UpsertCollection(ctx, col); err != nil {
		t.Fatal(err)
	}
	url := upstream.URL + "/browse/PAY-123"
	if err := m.webStore.UpsertPage(ctx, &websource.Page{ID: websource.PageID("support", "pay-123"), Collection: "support", Slug: "pay-123", URL: url, Status: websource.StatusError, IndexDirty: true}); err != nil {
		t.Fatal(err)
	}
	m.sourcesRootDir = t.TempDir()
	dir := m.sourcesDir("support")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pay-123.md"), []byte("# PAY-123\nStored incident context."), 0o600); err != nil {
		t.Fatal(err)
	}
	// Generated-repo docs can be disabled while synced source reads still work.
	read, err := m.GetDocDetails(ctx, "web:support", "pay-123.md", 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if read.URL != url || read.CollectionType != "jira" || read.Evidence.Kind != "synced_snapshot" || !read.Evidence.IndexPending || read.Evidence.ItemStatus != websource.StatusError || !read.Evidence.CollectionRefreshedAt.Equal(refreshed) {
		t.Fatalf("read=%+v", read)
	}
	docs := []rag.Doc{{Repo: "web:support", Path: "pay-123.md"}}
	m.enrichDocSources(ctx, docs)
	if docs[0].Evidence != read.Evidence || docs[0].URL != read.URL {
		t.Fatal("search and read provenance diverged")
	}
	if requests.Load() != 0 {
		t.Fatal("snapshot read contacted the live provider")
	}
	note := m.emptyDocsNote(DocsSearchLexical, ScopeSources, "web:support", "")
	if !strings.Contains(note, "does not prove") || !strings.Contains(note, "live Jira/Confluence") {
		t.Fatalf("empty-result guidance=%s", note)
	}
}
