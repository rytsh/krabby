package mcptools

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/websource"
)

type tracingService interface {
	Tracer() *langfuse.Tracer
}

type activityService interface {
	Activity(string) string
}

type repoReadService interface {
	ListRepos(context.Context, registry.ListOptions) ([]*registry.Repo, int, error)
	RepoNamespaces(context.Context) ([]registry.NamespaceGroup, error)
	Repo(context.Context, string) (*registry.Repo, error)
	Activity(string) string
}

type repoAdminService interface {
	AddRepo(context.Context, manager.RepoSpec, ...string) (*registry.Repo, error)
	AddRepoWait(context.Context, manager.RepoSpec, ...string) (*registry.Repo, bool, error)
	SetRepoNamespace(context.Context, string, string) (*registry.Repo, error)
	SetRepoOverrides(context.Context, string, registry.Overrides) (*registry.Repo, error)
	UpsertNamespace(context.Context, string, types.Null[string]) (*registry.NamespaceRecord, error)
	DeleteNamespace(context.Context, string) error
	RemoveRepo(context.Context, string) error
	TriggerGenerate(string, []string, bool) error
	GenerateWait(context.Context, string, []string, bool) (*registry.Repo, bool, error)
	TriggerRefresh(string, ...string) error
	RefreshWait(context.Context, string, ...string) (*registry.Repo, bool, error)
	CancelJob(string) bool
	Activity(string) string
}

type queueService interface {
	TaskSnapshot() queue.Snapshot
	BumpTask(uint64) bool
	CancelTask(uint64) bool
	CancelTasks(string) int
	SetTaskConcurrency(int)
}

type graphQueryService interface {
	CallGraphTool(context.Context, string, string, string, map[string]any) (*mcp.CallToolResult, error)
	FindDefinition(context.Context, string, string, string, int) (graphquery.SymbolDefs, error)
	FindReferences(context.Context, string, string, string, []string, int, int) (graphquery.SymbolRefs, error)
}

type repoFileService interface {
	ReadRepoFileAt(context.Context, string, string, string, int64, int) (*repofs.FileContent, error)
	ListRepoFilesPageAt(context.Context, string, string, string, bool, string, int) (repofs.EntryPage, error)
	GlobRepoFiles(context.Context, string, string, string, int) (repofs.GlobPage, error)
	BlameRepoFile(context.Context, string, string, string, int, int) (*manager.BlameFileResult, error)
}

type repoHistoryService interface {
	RepoRefs(context.Context, string, string, int) (*manager.RepoRefsResult, error)
	RepoLog(context.Context, string, gitops.LogOptions) (*manager.RepoLogResult, error)
	RepoDiff(context.Context, string, string, string, string, bool) (*manager.RepoDiffResult, error)
}

type docsSearchService interface {
	SearchDocs(context.Context, string, string, string, string, string, int) (rag.DocsPage, error)
	SearchCodeText(context.Context, string, string, string, coderag.TextSearchOptions) (coderag.SearchPage, error)
	SearchCodeRegex(context.Context, string, string, string, coderag.RegexOptions) (coderag.RegexPage, error)
	SearchCode(context.Context, string, string, string, coderag.SemanticOptions) (coderag.SemanticPage, error)
	ListDocs(context.Context, string) ([]docgen.DocMeta, error)
	GetDoc(context.Context, string, string, int64, int) (*repofs.FileContent, error)
}

type sourceReadService interface {
	ListWebCollections(context.Context) ([]*websource.Collection, error)
	WebCollection(context.Context, string) (*websource.Collection, error)
	WebPagesPaged(context.Context, string, string, string, int, int) ([]*websource.Page, int, error)
	Activity(string) string
	Progress(string) ([]manager.Progress, bool)
}

type sourceAdminService interface {
	sourceReadService
	WebSourceTypes() []string
	WebSourceConfigView(*websource.Collection) any
	AddWebCollection(context.Context, *websource.Collection) error
	UpdateWebCollection(context.Context, string, websource.CollectionUpdate, json.RawMessage) error
	DeleteWebCollection(context.Context, string) error
	TriggerWebRefresh(string) error
	AddWebPage(context.Context, string, string) (*websource.Page, error)
	ImportWebPages(context.Context, string, []manager.WebPageImport) (manager.WebPageImportResult, error)
	ImportWebSitemap(context.Context, string, string) (manager.SitemapImportResult, error)
	DeleteWebPage(context.Context, string, string) error
}

type docsSettingsService interface {
	GetDocsConfig(context.Context) (settings.Redacted, error)
	PatchDocsConfig(context.Context, settings.Patch) (settings.Redacted, error)
	TestLLM(context.Context, settings.Settings) manager.TestResult
	TestEmbedder(context.Context, settings.Settings) manager.TestResult
	TestCodeEmbedder(context.Context, settings.Settings) manager.TestResult
}

type apiReadService interface {
	APIGroups(context.Context) ([]apicatalog.GroupSummary, error)
	APIServicesPaged(context.Context, string, string, int, int) ([]*apicatalog.Service, int, error)
	APIOperationsPaged(context.Context, string, string, string, string, int, int) ([]*apicatalog.Operation, int, error)
	APITags(context.Context, string) ([]string, error)
	APIOperation(context.Context, string, string) (*apicatalog.Operation, error)
	CallAPIOperation(context.Context, string, string, apicatalog.CallRequest) (apicatalog.CallResponse, error)
}

type apiAdminService interface {
	APIServiceKinds() []string
	UpsertAPIGroup(context.Context, string, string) (*apicatalog.Group, error)
	DeleteAPIGroup(context.Context, string) error
	AddAPIService(context.Context, *apicatalog.Service) error
	UpdateAPIService(context.Context, string, apicatalog.ServiceUpdate, json.RawMessage) error
	DeleteAPIService(context.Context, string) error
	APIService(context.Context, string) (*apicatalog.Service, error)
	TriggerAPIRefresh(string) error
	APIServiceConfigView(*apicatalog.Service) any
	Activity(string) string
}

type credentialService interface {
	SetCredential(context.Context, *credentials.Credential) error
	ListCredentials(context.Context) ([]*credentials.Credential, error)
	DeleteCredential(context.Context, string) error
}

var (
	_ tracingService      = (*manager.Manager)(nil)
	_ activityService     = (*manager.Manager)(nil)
	_ repoReadService     = (*manager.Manager)(nil)
	_ repoAdminService    = (*manager.Manager)(nil)
	_ queueService        = (*manager.Manager)(nil)
	_ graphQueryService   = (*manager.Manager)(nil)
	_ repoFileService     = (*manager.Manager)(nil)
	_ repoHistoryService  = (*manager.Manager)(nil)
	_ docsSearchService   = (*manager.Manager)(nil)
	_ sourceReadService   = (*manager.Manager)(nil)
	_ sourceAdminService  = (*manager.Manager)(nil)
	_ docsSettingsService = (*manager.Manager)(nil)
	_ apiReadService      = (*manager.Manager)(nil)
	_ apiAdminService     = (*manager.Manager)(nil)
	_ credentialService   = (*manager.Manager)(nil)
)
