package manager

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/apicatalog"
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
		{name: "apis", scope: ScopeAPIs, want: vectorstore.Filter{Kind: vectorstore.KindAPI}},
		{name: "single api wins", scope: ScopeRepos, key: "api:billing", want: vectorstore.Filter{Keys: []string{"api:billing"}}},
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
	if err := mgr.apiStore.UpsertService(ctx, &apicatalog.Service{Name: "billing", Kind: "openapi"}); err != nil {
		t.Fatal(err)
	}
	got, empty, err = mgr.docsNamespaceFilter(ctx, ScopeAll, "", "payments", vectorstore.Filter{})
	// A catalogued API belongs to no repository namespace, so leaving it out
	// of the allow-list would make a default scope=all search structurally
	// incapable of returning an endpoint document.
	want = vectorstore.Filter{Keys: []string{"acme/payments", "acme/payments-worker", "api:billing", "web:wiki"}}
	if err != nil || empty || !reflect.DeepEqual(got, want) {
		t.Fatalf("all filter = %#v, empty=%t err=%v; want %#v", got, empty, err, want)
	}
	// scope=apis selects no repository, so a namespace has nothing to narrow
	// and the kind filter must survive untouched.
	apiFilter := vectorstore.Filter{Kind: vectorstore.KindAPI}
	got, empty, err = mgr.docsNamespaceFilter(ctx, ScopeAPIs, "", "payments", apiFilter)
	if err != nil || empty || !reflect.DeepEqual(got, apiFilter) {
		t.Fatalf("apis filter = %#v, empty=%t err=%v; want %#v", got, empty, err, apiFilter)
	}
}

// TestNamespaceFilterKeepsUnnamespacedDocs pins the post-retrieval half of the
// same rule: web sources and catalogued APIs are not in any namespace's repo
// set, so matching them against it drops every one of their hits.
func TestNamespaceFilterKeepsUnnamespacedDocs(t *testing.T) {
	t.Parallel()
	mgr, reg := newNamespaceManager(t)
	ctx := context.Background()
	for _, repo := range []*registry.Repo{
		{ID: "acme/payments", Namespace: "payments"},
		{ID: "acme/other", Namespace: "other"},
	} {
		if err := reg.Upsert(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}

	docs := []rag.Doc{
		{Repo: "acme/payments", Path: "a.md"},
		{Repo: "acme/other", Path: "b.md"},
		{Repo: "web:wiki", Path: "c.md"},
		{Repo: "api:billing", Path: "d.md"},
	}
	got, err := mgr.filterDocsByNamespace(ctx, docs, "payments", 0)
	if err != nil {
		t.Fatal(err)
	}

	var kept []string
	for _, doc := range got {
		kept = append(kept, doc.Repo)
	}
	want := []string{"acme/payments", "web:wiki", "api:billing"}
	if !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
}

// TestAPIDocMetadataAndScopeResolution covers the rest of the API-catalog
// search path: a hit must identify itself as an API rather than a repository,
// the scope key must validate, and get_doc must be able to follow it.
func TestAPIDocMetadataAndScopeResolution(t *testing.T) {
	t.Parallel()
	mgr, _ := newNamespaceManager(t)
	ctx := context.Background()

	if err := mgr.validateDocsKey(ctx, "api:missing"); err == nil || !strings.Contains(err.Error(), "list_api_services") {
		t.Fatalf("unknown api scope error = %v", err)
	}
	if err := mgr.apiStore.UpsertService(ctx, &apicatalog.Service{
		Name: "billing", Kind: "openapi", Group: "payments",
		SpecSummary: "from the spec", ResolvedBaseURL: "https://billing.internal",
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.validateDocsKey(ctx, "api:billing"); err != nil {
		t.Fatalf("known api scope rejected: %v", err)
	}

	docs := []rag.Doc{{Repo: "api:billing", Path: "get-v1-invoices.md"}}
	mgr.enrichDocSources(ctx, docs)
	if docs[0].SourceKind != "api" || docs[0].ScopeKey != "api:billing" ||
		docs[0].ServiceName != "billing" || docs[0].ServiceGroup != "payments" ||
		docs[0].ServiceBaseURL != "https://billing.internal" {
		t.Fatalf("api metadata = %#v", docs[0])
	}
	// A repo namespace must never be invented for a source that has none.
	if docs[0].Namespace != "" {
		t.Errorf("api hit carries namespace %q", docs[0].Namespace)
	}
	// With no human override the specification's own summary is shown.
	if docs[0].ServiceDescription != "from the spec" {
		t.Errorf("description = %q, want the spec summary", docs[0].ServiceDescription)
	}

	mgr.apisRootDir = t.TempDir()
	if _, err := mgr.repoDocsDir(ctx, "api:../../../etc"); err == nil || !strings.Contains(err.Error(), "invalid api scope") {
		t.Fatalf("unsafe api scope error = %v", err)
	}
	if _, err := mgr.repoDocsDir(ctx, "api:unknown"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown api read scope error = %v", err)
	}
	want := mgr.apisDir("billing")
	if got, err := mgr.repoDocsDir(ctx, "api:billing"); err != nil || got != want {
		t.Fatalf("api docs dir = %q, err=%v; want %q", got, err, want)
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
