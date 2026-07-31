package manager

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
)

func TestDocsFilter(t *testing.T) {
	tests := []struct {
		name, scope, key string
		want             vectorstore.Filter
		wantErr          bool
	}{
		{name: "all default", want: vectorstore.Filter{}},
		{name: "all explicit", scope: ScopeAll, want: vectorstore.Filter{}},
		{name: "repos", scope: ScopeRepos, want: vectorstore.Filter{Kind: vectorstore.KindRepo}},
		{name: "sources", scope: ScopeSources, want: vectorstore.Filter{Kind: vectorstore.KindWeb}},
		{name: "single source wins", scope: ScopeRepos, key: "web:wine", want: vectorstore.Filter{Keys: []string{"web:wine"}}},
		{name: "single repo", key: "git.example.com/a/repo", want: vectorstore.Filter{Keys: []string{"git.example.com/a/repo"}}},
		{name: "invalid", scope: "other", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := docsFilter(tt.scope, tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("filter=%#v want=%#v", got, tt.want)
			}
		})
	}
}

func TestDocsNamespaceFilterPushesExactKeysIntoRetrieval(t *testing.T) {
	t.Parallel()
	mgr, reg := newNamespaceManager(t)
	ctx := context.Background()
	for _, repo := range []*registry.Repo{
		{ID: "acme/default"},
		{ID: "acme/payments", Namespace: "payments"},
		{ID: "acme/payments-worker", Namespace: "payments"},
	} {
		if err := reg.Upsert(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	got, empty, err := mgr.docsNamespaceFilter(ctx, ScopeRepos, "", "payments", vectorstore.Filter{Kind: vectorstore.KindRepo})
	if err != nil {
		t.Fatal(err)
	}
	want := vectorstore.Filter{Keys: []string{"acme/payments", "acme/payments-worker"}}
	if empty || !reflect.DeepEqual(got, want) {
		t.Fatalf("filter = %#v, empty=%t; want %#v", got, empty, want)
	}
	_, empty, err = mgr.docsNamespaceFilter(ctx, ScopeRepos, "", "missing", vectorstore.Filter{Kind: vectorstore.KindRepo})
	if err != nil || !empty {
		t.Fatalf("missing namespace: empty=%t err=%v", empty, err)
	}
	if err := mgr.webStore.UpsertCollection(ctx, &websource.Collection{Name: "wiki", Type: websource.TypePages}); err != nil {
		t.Fatal(err)
	}
	got, empty, err = mgr.docsNamespaceFilter(ctx, ScopeAll, "", "payments", vectorstore.Filter{})
	want = vectorstore.Filter{Keys: []string{"acme/payments", "acme/payments-worker", "web:wiki"}}
	if err != nil || empty || !reflect.DeepEqual(got, want) {
		t.Fatalf("all filter = %#v, empty=%t err=%v; want %#v", got, empty, err, want)
	}
}

func TestDocsScopeValidationAndResultMetadata(t *testing.T) {
	t.Parallel()
	mgr, reg := newNamespaceManager(t)
	ctx := context.Background()
	if err := reg.Upsert(ctx, &registry.Repo{ID: "host/team/api", Namespace: "payments"}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.validateDocsKey(ctx, "host/team/missing"); err == nil || !strings.Contains(err.Error(), "list_repos") {
		t.Fatalf("unknown repo error = %v", err)
	}
	if err := mgr.validateDocsKey(ctx, "host/team/api"); err != nil {
		t.Fatalf("known repo rejected: %v", err)
	}
	if err := mgr.validateDocsKey(ctx, "web:missing"); err == nil || !strings.Contains(err.Error(), "list_sources") {
		t.Fatalf("unknown web scope error = %v", err)
	}
	docs := []rag.Doc{{Repo: "host/team/api", Path: "overview.md"}}
	mgr.enrichDocSources(ctx, docs)
	if docs[0].SourceKind != "repository" || docs[0].ScopeKey != "host/team/api" || docs[0].Namespace != "payments" {
		t.Fatalf("metadata = %#v", docs[0])
	}
	if err := mgr.webStore.UpsertCollection(ctx, &websource.Collection{
		Name: "support", Type: websource.TypeConfluence, Description: "Support runbooks",
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.validateDocsKey(ctx, "web:support"); err != nil {
		t.Fatalf("known web scope rejected: %v", err)
	}
	mgr.sourcesRootDir = t.TempDir()
	if _, err := mgr.repoDocsDir(ctx, "web:../../../etc"); err == nil || !strings.Contains(err.Error(), "invalid web source scope") {
		t.Fatalf("unsafe web scope error = %v", err)
	}
	if _, err := mgr.repoDocsDir(ctx, "web:unknown"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown web read scope error = %v", err)
	}
	wantSourceDir := mgr.sourcesDir("support")
	if got, err := mgr.repoDocsDir(ctx, "web:support"); err != nil || got != wantSourceDir {
		t.Fatalf("known web docs dir = %q, err=%v; want %q", got, err, wantSourceDir)
	}
	if err := mgr.webStore.UpsertPage(ctx, &websource.Page{
		ID: websource.PageID("support", "incident"), Collection: "support", Slug: "incident",
		URL: "https://wiki.example/incident", Teams: []string{"SRE"},
	}); err != nil {
		t.Fatal(err)
	}
	webDocs := []rag.Doc{{Repo: "web:support", Path: "incident.md"}}
	mgr.enrichDocSources(ctx, webDocs)
	if webDocs[0].SourceKind != "web" || webDocs[0].ScopeKey != "web:support" ||
		webDocs[0].CollectionName != "support" || webDocs[0].CollectionType != websource.TypeConfluence ||
		webDocs[0].CollectionDescription != "Support runbooks" || webDocs[0].URL == "" {
		t.Fatalf("web metadata = %#v", webDocs[0])
	}
}
