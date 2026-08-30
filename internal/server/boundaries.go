package server

import (
	"context"
	"encoding/json"

	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/websource"
)

type systemInfoService interface {
	GraphifyVersion() string
}

type repoReader interface {
	Repo(context.Context, string) (*registry.Repo, error)
	ResolveRepo(context.Context, string) (*registry.Repo, error)
	ListRepos(context.Context, registry.ListOptions) ([]*registry.Repo, int, error)
	RepoOwners(context.Context) ([]registry.OwnerGroup, error)
	RepoNamespaces(context.Context) ([]registry.NamespaceGroup, error)
	Activity(string) string
	ActiveRepos() map[string]string
	Progress(string) ([]manager.Progress, bool)
}

type repoAdmin interface {
	AddRepo(context.Context, manager.RepoSpec, ...string) (*registry.Repo, error)
	RemoveRepo(context.Context, string) error
	SetRepoNamespace(context.Context, string, string) (*registry.Repo, error)
	SetRepoOverrides(context.Context, string, registry.Overrides) (*registry.Repo, error)
	RepoSettings(context.Context, string) (*manager.RepoSettings, error)
	UpsertNamespace(context.Context, string, types.Null[string]) (*registry.NamespaceRecord, error)
	DeleteNamespace(context.Context, string) error
	TriggerRefresh(string, ...string) error
	TriggerGenerate(string, []string, bool) error
	CancelJob(string) bool
}

type repoService interface {
	repoReader
	repoAdmin
}

type queueService interface {
	TaskSnapshot() queue.Snapshot
	ClearTaskHistory()
	CancelPendingTasks() int
	SetTaskConcurrency(int)
	BumpTask(uint64) bool
	CancelTask(uint64) bool
}

type docsService interface {
	ListRepoFilesAt(context.Context, string, string, string, bool) ([]repofs.Entry, string, error)
	ReadRepoFileAt(context.Context, string, string, string, int64, int) (*repofs.FileContent, error)
	ListDocs(context.Context, string) ([]docgen.DocMeta, error)
	GetDoc(context.Context, string, string, int64, int) (*repofs.FileContent, error)
	SearchDocs(context.Context, string, string, string, string, string, int) ([]rag.Doc, error)
	SearchCodeText(context.Context, string, string, string, int, int) (coderag.SearchPage, error)
	SearchCode(context.Context, string, string, string, int) ([]coderag.Snippet, error)
	MergedPath() string
}

type docsSettingsService interface {
	GetDocsConfig(context.Context) (settings.Redacted, error)
	PatchDocsConfig(context.Context, settings.Patch) (settings.Redacted, error)
	TestLLM(context.Context, settings.Settings) manager.TestResult
	TestEmbedder(context.Context, settings.Settings) manager.TestResult
	TestCodeEmbedder(context.Context, settings.Settings) manager.TestResult
	TestLangfuse(context.Context, settings.Settings) manager.TestResult
}

type sourceService interface {
	ListWebCollections(context.Context) ([]*websource.Collection, error)
	WebCollection(context.Context, string) (*websource.Collection, error)
	WebPagesPaged(context.Context, string, string, string, int, int) ([]*websource.Page, int, error)
	WebSourceTeams(context.Context, string) ([]string, error)
	WebPageCount(context.Context, string) (int, error)
	WebPage(context.Context, string, string) (*websource.Page, error)
	WebSourceConfigView(*websource.Collection) any
	TestWebSource(context.Context, string, string, json.RawMessage) manager.WebSourceTestResult
	AddWebCollection(context.Context, *websource.Collection) error
	UpdateWebCollection(context.Context, string, websource.CollectionUpdate, json.RawMessage) error
	DeleteWebCollection(context.Context, string) error
	TriggerWebRefresh(string) error
	AddWebPage(context.Context, string, string) (*websource.Page, error)
	ImportWebPages(context.Context, string, []manager.WebPageImport) (manager.WebPageImportResult, error)
	ImportWebSitemap(context.Context, string, string) (manager.SitemapImportResult, error)
	DeleteWebPage(context.Context, string, string) error
	WebSourceDoc(context.Context, string, string) (*repofs.FileContent, error)
	Activity(string) string
	Progress(string) ([]manager.Progress, bool)
	TaskState(string) string
	CancelTasks(string) int
}

type apiCatalogService interface {
	APIGroups(context.Context) ([]apicatalog.GroupSummary, error)
	UpsertAPIGroup(context.Context, string, string) (*apicatalog.Group, error)
	DeleteAPIGroup(context.Context, string) error
	APIServicesPaged(context.Context, string, string, int, int) ([]*apicatalog.Service, int, error)
	AddAPIService(context.Context, *apicatalog.Service) error
	APIService(context.Context, string) (*apicatalog.Service, error)
	APIOperationsPaged(context.Context, string, string, string, string, int, int) ([]*apicatalog.Operation, int, error)
	APITags(context.Context, string) ([]string, error)
	APIOperation(context.Context, string, string) (*apicatalog.Operation, error)
	CallAPIOperation(context.Context, string, string, apicatalog.CallRequest) (apicatalog.CallResponse, error)
	UpdateAPIService(context.Context, string, apicatalog.ServiceUpdate, json.RawMessage) error
	DeleteAPIService(context.Context, string) error
	TriggerAPIFullRefresh(string) error
	TriggerAPIRefresh(string) error
	TestAPIServiceConfig(context.Context, string, string, json.RawMessage, json.RawMessage) (apicatalog.PreviewResult, error)
	APIServiceKinds() []string
	APIServiceConfigView(*apicatalog.Service) any
	Activity(string) string
	Progress(string) ([]manager.Progress, bool)
	TaskState(string) string
	CancelTasks(string) int
}

type credentialService interface {
	ListCredentials(context.Context) ([]*credentials.Credential, error)
	SetCredential(context.Context, *credentials.Credential) error
	DeleteCredential(context.Context, string) error
}

type tracingService interface {
	Tracer() *langfuse.Tracer
}

type webhookService interface {
	WebhookSecret() string
	ResolveRepo(context.Context, string) (*registry.Repo, error)
	TriggerRefresh(string, ...string) error
}

type routeServices struct {
	system      systemInfoService
	repos       repoService
	queue       queueService
	docs        docsService
	docsConfig  docsSettingsService
	sources     sourceService
	apis        apiCatalogService
	credentials credentialService
	tracing     tracingService
	webhook     webhookService
}

func managerRouteServices(mgr *manager.Manager) routeServices {
	return routeServices{
		system: mgr, repos: mgr, queue: mgr, docs: mgr,
		docsConfig: mgr, sources: mgr, apis: mgr, credentials: mgr,
		tracing: mgr, webhook: mgr,
	}
}

var (
	_ systemInfoService   = (*manager.Manager)(nil)
	_ repoReader          = (*manager.Manager)(nil)
	_ repoAdmin           = (*manager.Manager)(nil)
	_ queueService        = (*manager.Manager)(nil)
	_ docsService         = (*manager.Manager)(nil)
	_ docsSettingsService = (*manager.Manager)(nil)
	_ sourceService       = (*manager.Manager)(nil)
	_ apiCatalogService   = (*manager.Manager)(nil)
	_ credentialService   = (*manager.Manager)(nil)
	_ tracingService      = (*manager.Manager)(nil)
	_ webhookService      = (*manager.Manager)(nil)
)
