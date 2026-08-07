// Package mcptools exposes krabby's MCP server: repo management tools plus
// graph query tools proxied to per-graph graphify servers.
package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/registry"
)

// serverInstructions is the server-level guidance returned to clients on
// initialize. Most MCP clients surface it to the LLM as high-level context, so
// it explains what krabby is, the add->poll->query lifecycle, and which tool to
// reach for first. Per-tool specifics stay in each tool's Description.
const serverInstructions = `Krabby tracks git repositories and builds a searchable knowledge graph over each one, so you can locate code, read the actual source from the clone, understand how it fits together, attribute changes to commits, and search its documentation - without cloning anything yourself.

Tool selection (roughly what to reach for, in order):
- Use search_code first for symbols, paths, literals, definitions, usages, and implementation locations. Use normal mode for exact text and semantic mode for conceptual source search.
- Use read_file to view the actual source behind a result (node 'src' fields give the path); reads are sandboxed to the clone and paginated.
- Use query_graph for architecture, dependencies, call/data flow, and relationships across files. It is not a keyword or symbol search.
- Use git_blame to attribute lines to a commit (start_line/end_line blames a range), then git_diff with that sha to see what it changed.
- Use git_log with from/to to compare releases or follow one file; list_refs gives tag names.
- Use search_docs for documentation and knowledge; it covers generated repo docs, web sources (Confluence, Jira, pages) and catalogued API endpoints. Semantic is the default when configured, otherwise lexical; request hybrid explicitly for fused retrieval. Use lexical for exact keys/titles/identifiers and semantic for conceptual questions. Pass the user's full question. Scope a web source with the exact scope_key returned by list_sources.
- To call an API, walk the catalog: list_api_groups -> list_api_services -> list_api_endpoints -> get_api_endpoint. Only the last returns schemas; narrow with search/tag rather than listing every endpoint.
- Use list_* only when an identifier is unknown or the user explicitly requests an inventory. Do not exhaust pages or request a recursive file tree without a clear need.
- Use get_* tools only after a search/query identifies the target.
- If a graph tool returns "Repository selection required", retry it with one of the provided repo ids instead of treating the result as a failure.

Always pass repo when it is known. Omit repo only when the user explicitly requests cross-repository analysis and merged search is intended.

Repos are grouped into namespaces. When the repo is unknown a search covers only the 'default' namespace, so the answer may live elsewhere: before concluding nothing was found, check list_namespaces and pass the matching namespace, or namespace:'*' to search them all.

add_repo and refresh_repo run in the background by default. Poll repo_status until ready or error before querying.`

const (
	ToolProfileStandard = "standard"
	ToolProfileFull     = "full"
)

// New builds the MCP server with all krabby tools registered. waitTimeout caps
// how long wait=true management calls block before returning the in-progress
// status (the build keeps running in the background); <=0 means no server-side
// cap.
func New(mgr *manager.Manager, version string, waitTimeout time.Duration, profile string) *mcp.Server {
	title := "Krabby codebase search and knowledge"
	instructions := serverInstructions
	if profile == ToolProfileFull {
		title += " (full administration)"
		instructions += "\n\nThis connection uses the full profile and can mutate credentials, runtime configuration, web sources and the API catalog. Collections use add/update/refresh/delete_source and get_source_config; pages-source items use register_source_page, import_source_pages, import_source_sitemap and delete_source_page. API services use add/update/refresh/delete_api_service and get_api_service_config. Use mutation tools only when explicitly requested."
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "krabby",
		Title:   title,
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	// One receiving middleware covers every tool, so instrumentation cannot
	// drift out of sync as tools are added.
	server.AddReceivingMiddleware(traceMiddleware(mgr))

	addManagementTools(server, mgr, waitTimeout)
	addQueryTools(server, mgr)
	addFileTools(server, mgr)
	addHistoryTools(server, mgr)
	addDocTools(server, mgr, profile == ToolProfileFull)
	addAPITools(server, mgr, profile == ToolProfileFull)
	if profile == ToolProfileFull {
		addCredentialTools(server, mgr)
	}

	return server
}

// ---- management tools -------------------------------------------------------

type addRepoArgs struct {
	URL       string `json:"url" jsonschema:"git URL of the repository (ssh or https)"`
	Branch    string `json:"branch,omitempty" jsonschema:"branch to track (default: repo default branch)"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace to assign this repo to (arbitrary grouping label); omitted means the 'default' namespace. '*' is reserved and rejected. Ignored if the repo is already tracked (use set_repo_namespace to move it)"`
	Wait      bool   `json:"wait,omitempty" jsonschema:"when true, block until the clone and graph build finish and return the final status (ready or error) instead of returning immediately"`

	repoOverrideArgs
}

// repoOverrideArgs are the per-repository overrides of the install-wide file
// selection and documentation prompt, shared by add_repo and
// set_repo_overrides.
type repoOverrideArgs struct {
	Include      []string `json:"include,omitempty" jsonschema:"globs REPLACING the built-in file allowlist for this repo"`
	IncludeExtra []string `json:"include_extra,omitempty" jsonschema:"globs ADDED to the allowlist, e.g. ['**/*.yaml']; prefer this over include"`
	Exclude      []string `json:"exclude,omitempty" jsonschema:"globs skipped; applied last and wins"`
	GraphExclude []string `json:"graph_exclude,omitempty" jsonschema:"patterns kept out of the knowledge graph; rebuilds it"`

	DocsPrompt      string `json:"docs_prompt,omitempty" jsonschema:"prompt REPLACING the default documentation prompt; prefer docs_prompt_extra"`
	DocsPromptExtra string `json:"docs_prompt_extra,omitempty" jsonschema:"extra documentation instructions for this repo"`

	DocsMaxSourceBytes    int `json:"docs_max_source_bytes,omitempty" jsonschema:"bytes of ONE file sent to the LLM (default 49152); raise it when the repo's substance sits in a few very large files, which are otherwise documented from a truncated prefix. 0 inherits"`
	DocsMaxGroupBytes     int `json:"docs_max_group_bytes,omitempty" jsonschema:"source bytes per grouped summary call (default 98304), split across the group's files; raise with docs_max_source_bytes or it binds first. 0 inherits"`
	DocsMaxSynthesisBytes int `json:"docs_max_synthesis_bytes,omitempty" jsonschema:"summary bytes fed to the final documentation.md synthesis (default 262144); 0 inherits"`

	SkipStages []string `json:"skip_stages,omitempty" jsonschema:"stages this repo does not run: graph, docs, docs_index, code_index. Dependents still run degraded (docs summarize per file without a graph). Requesting a skipped stage is an error, not a no-op"`
}

func (a repoOverrideArgs) overrides() registry.Overrides {
	return registry.Overrides{
		Include:         a.Include,
		IncludeExtra:    a.IncludeExtra,
		Exclude:         a.Exclude,
		GraphExclude:    a.GraphExclude,
		DocsPrompt:      a.DocsPrompt,
		DocsPromptExtra: a.DocsPromptExtra,

		DocsMaxSourceBytes:    a.DocsMaxSourceBytes,
		DocsMaxGroupBytes:     a.DocsMaxGroupBytes,
		DocsMaxSynthesisBytes: a.DocsMaxSynthesisBytes,

		SkipStages: a.SkipStages,
	}
}

type setRepoOverridesArgs struct {
	Repo string `json:"repo" jsonschema:"repository id (owner/name) to configure"`

	repoOverrideArgs
}

type repoIDArgs struct {
	Repo string `json:"repo" jsonschema:"repository id in owner/name form"`
}

type refreshRepoArgs struct {
	Repo   string   `json:"repo" jsonschema:"repository id in owner/name form"`
	Wait   bool     `json:"wait,omitempty" jsonschema:"when true, block until the pull and graph rebuild finish and return the final status (ready or error) instead of returning immediately"`
	Stages []string `json:"stages,omitempty" jsonschema:"optional subset of pipeline stages to rebuild against the existing clone without pulling git: graph, docs, docs_index, code_index. Empty runs the full pull+rebuild pipeline. Use e.g. ['docs_index'] to re-embed docs after they were regenerated. Missing prerequisites (docs_index needs docs, which needs graph) are built automatically only when their output is absent"`
	Force  bool     `json:"force,omitempty" jsonschema:"when true, the docs stage ignores its incremental caches and regenerates every per-file summary and documentation.md even if nothing changed. Requires stages to include 'docs' (otherwise docs are reused because unchanged). Ignored by the full pull+rebuild pipeline (empty stages)"`
	Skip   []string `json:"skip,omitempty" jsonschema:"stages the full pull+rebuild must NOT run this time, e.g. ['docs'] to pull commits and refresh the code index without paying for LLM doc generation. Skipping 'docs' also skips 'docs_index'. This run only; not combinable with stages"`
}

// validateStages rejects unknown stage names so a typo fails fast with a clear
// message instead of silently doing nothing. An empty stages list is valid and
// selects the full pull+rebuild pipeline.
func (a refreshRepoArgs) validateStages() error {
	for _, s := range a.Stages {
		if !registry.ValidStage(s) {
			return unknownStageError(s)
		}
	}

	return nil
}

// validateSkip rejects unknown stage names in the per-run skip list. It also
// rejects combining skip with stages: stages is an allow-list evaluated against
// the existing clone, skip a deny-list on the full pull+rebuild pipeline, so a
// request carrying both has no single meaning worth guessing at.
func (a refreshRepoArgs) validateSkip() error {
	if len(a.Skip) > 0 && len(a.Stages) > 0 {
		return fmt.Errorf("skip and stages cannot be combined; stages already selects exactly what runs")
	}

	for _, s := range a.Skip {
		if !registry.ValidStage(s) {
			return unknownStageError(s)
		}
	}

	return nil
}

func unknownStageError(s string) error {
	return fmt.Errorf("unknown stage %q; valid stages are: %s, %s, %s, %s",
		s, registry.StageGraph, registry.StageDocs, registry.StageDocsIndex, registry.StageCodeIndex)
}

type emptyArgs struct{}

type listReposArgs struct {
	Page      int    `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage   int    `json:"per_page,omitempty" jsonschema:"results per page (default 20, max 200)"`
	Search    string `json:"search,omitempty" jsonschema:"case-insensitive substring filter on the repo id (host/group/.../name)"`
	Owner     string `json:"owner,omitempty" jsonschema:"restrict to the direct children of one directory prefix (everything before the repo name)"`
	Namespace string `json:"namespace,omitempty" jsonschema:"restrict to one namespace; empty or 'default' selects the default bucket (untagged repos), '*' lists every namespace"`
}

type setRepoNamespaceArgs struct {
	Repo      string `json:"repo" jsonschema:"repository id (owner/name) to move"`
	Namespace string `json:"namespace" jsonschema:"target namespace; empty or 'default' returns the repo to the default bucket. '*' is reserved and rejected"`
}

type upsertNamespaceArgs struct {
	Name        string `json:"name" jsonschema:"namespace name (arbitrary grouping label); empty or 'default' targets the default bucket. '*' is reserved and rejected"`
	Description string `json:"description,omitempty" jsonschema:"human/LLM-facing summary of what this namespace holds (e.g. 'payment services and their shared libraries'); shown by list_namespaces so models can pick the right scope"`
}

type namespaceNameArgs struct {
	Name string `json:"name" jsonschema:"namespace name to delete the description record for; the repos keep their tag"`
}

type namespaceListOutput struct {
	Namespaces []registry.NamespaceGroup `json:"namespaces"`
}

// repoView decorates a repo record with the transient in-memory activity so
// callers can see which pipeline step is currently running (empty = idle).
type repoView struct {
	*registry.Repo
	Running string `json:"running,omitempty"`
}

func viewRepo(mgr *manager.Manager, repo *registry.Repo) repoView {
	if repo == nil {
		return repoView{}
	}

	return repoView{Repo: trimRepoForView(repo), Running: mgr.Activity(repo.ID)}
}

// trimRepoForView drops the documentation prompts from a repo record.
//
// They are override *inputs*, not repository facts: a model listing
// repositories has no use for them, and a page of 20 repos each carrying a
// multi-kilobyte prompt would dominate the response. The glob lists stay — they
// are short and do tell the model what is searchable in that repo.
// get_docs_config strips the default prompt for the same reason.
//
// It copies first: repo points at the caller's record, so blanking the fields
// in place would corrupt what the registry handed out.
func trimRepoForView(repo *registry.Repo) *registry.Repo {
	trimmed := *repo
	trimmed.Overrides.DocsPrompt = ""
	trimmed.Overrides.DocsPromptExtra = ""

	return &trimmed
}

func addManagementTools(server *mcp.Server, mgr *manager.Manager, waitTimeout time.Duration) {
	addTool(server, &mcp.Tool{
		Name:        "list_repos",
		Description: "Discover tracked repository ids and build status when the target repo is unknown, or when the user asks for an inventory. Filter with search/owner/namespace and inspect one page; do not fetch every page routinely. Omitting namespace lists the 'default' bucket; pass namespace:'*' to list every namespace.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listReposArgs) (*mcp.CallToolResult, any, error) {
		opts := registry.ListOptions{
			Page:      args.Page,
			PerPage:   args.PerPage,
			Search:    args.Search,
			Owner:     args.Owner,
			Namespace: args.Namespace,
		}

		repos, total, err := mgr.Registry().ListPaged(ctx, opts)
		if err != nil {
			return nil, nil, err
		}

		views := make([]repoView, 0, len(repos))
		for _, repo := range repos {
			views = append(views, viewRepo(mgr, repo))
		}

		page, perPage := registry.PageParams(opts)

		return jsonResult(map[string]any{
			"repos":    views,
			"total":    total,
			"page":     page,
			"per_page": perPage,
		}), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "add_repo",
		Description: "Track a new repository: clones it and builds its knowledge graph. " +
			"By default returns immediately (status 'pending'); check progress with repo_status. " +
			"Pass wait=true to wait for the result: it returns the final status when the build finishes in time, " +
			"otherwise the in-progress status. The build always continues in the background even if the call " +
			"times out or is cancelled; poll repo_status until status is 'ready' or 'error'.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args addRepoArgs) (*mcp.CallToolResult, any, error) {
		if !args.Wait {
			repo, err := mgr.AddRepo(ctx, args.spec())
			if err != nil {
				return nil, nil, err
			}

			return jsonResult(repo), nil, nil
		}

		wctx, cancel := waitContext(ctx, waitTimeout)
		defer cancel()

		repo, done, err := mgr.AddRepoWait(wctx, args.spec())
		if err != nil {
			return nil, nil, err
		}

		return waitResult(mgr, repo, done), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "set_repo_namespace",
		Description: "Move a tracked repository into a namespace (an arbitrary grouping label). Omitting or passing 'default' returns it to the default bucket; '*' is reserved. This only re-tags the repo; it does not rebuild anything.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args setRepoNamespaceArgs) (*mcp.CallToolResult, any, error) {
		repo, err := mgr.SetRepoNamespace(ctx, args.Repo, args.Namespace)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(viewRepo(mgr, repo)), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "set_repo_overrides",
		Description: "Override the install-wide file selection and documentation prompt for ONE repository, for repos that do not fit the defaults: " +
			"a deployment repo holding compose/YAML rather than source (include_extra), or one whose docs need a specific shape (docs_prompt_extra). " +
			"The payload replaces the whole override set, so send every field you want to keep; an empty payload clears them. " +
			"A change rebuilds that repository's index and docs in the background.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args setRepoOverridesArgs) (*mcp.CallToolResult, any, error) {
		repo, err := mgr.SetRepoOverrides(ctx, args.Repo, args.overrides())
		if err != nil {
			return nil, nil, err
		}

		// The repo view strips the prompts to keep listings small, so echo the
		// stored override set here: this is the one call whose whole purpose is
		// to confirm what was written.
		return jsonResult(map[string]any{
			"repo":      viewRepo(mgr, repo),
			"overrides": repo.Overrides,
		}), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "list_namespaces",
		Description: "List the repository namespaces with their repo counts and descriptions. Untagged repos are reported under 'default'. Use it to discover which namespaces exist and what each holds before scoping a search with the namespace parameter; the description tells you which namespace matches the user's question.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, namespaceListOutput, error) {
		groups, err := mgr.Registry().Namespaces(ctx)
		if err != nil {
			return nil, namespaceListOutput{}, err
		}

		out := namespaceListOutput{Namespaces: groups}
		return jsonResult(out), out, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "set_namespace_description",
		Description: "Create or update a namespace's description (an arbitrary grouping label plus a summary of what it holds). The description is surfaced by list_namespaces to help pick the right search scope. This does not tag any repo; use add_repo or set_repo_namespace for that.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args upsertNamespaceArgs) (*mcp.CallToolResult, any, error) {
		// Presence comes from the raw arguments: a nullable typed field would
		// reflect into the tool schema as a {V,Valid} object.
		description, err := nullFromArgs(req.Params.Arguments, "description", args.Description)
		if err != nil {
			return nil, nil, err
		}

		rec, err := mgr.UpsertNamespace(ctx, args.Name, description)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(rec), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "delete_namespace",
		Description: "Delete a namespace's description record. Repos tagged with the namespace keep their tag; only the stored description is removed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args namespaceNameArgs) (*mcp.CallToolResult, any, error) {
		if err := mgr.DeleteNamespace(ctx, args.Name); err != nil {
			return nil, nil, err
		}

		return textResult("deleted namespace description " + args.Name), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "remove_repo",
		Description: "Stop tracking a repository and delete its local clone and graph.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args repoIDArgs) (*mcp.CallToolResult, any, error) {
		if err := mgr.RemoveRepo(ctx, args.Repo); err != nil {
			return nil, nil, err
		}

		return textResult("removed " + args.Repo), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "refresh_repo",
		Description: "Pull the latest commits and rebuild the knowledge graph for a repository. " +
			"By default rebuilds in the background and returns immediately. " +
			"The rebuild always continues in the background even if the call " +
			"times out or is cancelled; poll repo_status until status is 'ready' or 'error'. " +
			"Use when you know the repo changed. " +
			"Pass stages to rebuild only a subset (graph, docs, docs_index, code_index) against the existing " +
			"clone WITHOUT pulling git, or skip to run the full pull+rebuild minus some stages.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args refreshRepoArgs) (*mcp.CallToolResult, any, error) {
		if err := args.validateSkip(); err != nil {
			return nil, nil, err
		}

		if len(args.Stages) > 0 {
			if err := args.validateStages(); err != nil {
				return nil, nil, err
			}

			if !args.Wait {
				mgr.TriggerGenerate(args.Repo, args.Stages, args.Force)

				return textResult(fmt.Sprintf("generate %v queued for %s", args.Stages, args.Repo)), nil, nil
			}

			wctx, cancel := waitContext(ctx, waitTimeout)
			defer cancel()

			repo, done, err := mgr.GenerateWait(wctx, args.Repo, args.Stages, args.Force)
			if err != nil {
				return nil, nil, err
			}

			return waitResult(mgr, repo, done), nil, nil
		}

		if !args.Wait {
			mgr.TriggerRefresh(args.Repo, args.Skip...)

			if len(args.Skip) > 0 {
				return textResult(fmt.Sprintf("refresh queued for %s (skipping %v)", args.Repo, args.Skip)), nil, nil
			}

			return textResult("refresh queued for " + args.Repo), nil, nil
		}

		wctx, cancel := waitContext(ctx, waitTimeout)
		defer cancel()

		repo, done, err := mgr.RefreshWait(wctx, args.Repo, args.Skip...)
		if err != nil {
			return nil, nil, err
		}

		return waitResult(mgr, repo, done), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "repo_status",
		Description: "Get status of a tracked repository: build state, last commit, last error if any. " +
			"The 'running' field shows the pipeline step currently executing (e.g. 'sync', 'graph', 'docs'); " +
			"empty means no work is in flight. While status is 'pending' or 'building', poll again until it " +
			"becomes 'ready' or 'error'.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args repoIDArgs) (*mcp.CallToolResult, any, error) {
		repo, err := mgr.Registry().Get(ctx, args.Repo)
		if err != nil {
			return nil, nil, err
		}

		if repo == nil {
			return nil, nil, fmt.Errorf("repo %s not found", args.Repo)
		}

		return jsonResult(viewRepo(mgr, repo)), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "cancel_repo_job",
		Description: "Cancel the refresh/generate job currently running for a repository. " +
			"The in-flight step is aborted and recorded as 'cancelled by user'; the repo can be " +
			"refreshed again later. Fails if no job is running (check the 'running' field of repo_status).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args repoIDArgs) (*mcp.CallToolResult, any, error) {
		if !mgr.CancelJob(args.Repo) {
			return nil, nil, fmt.Errorf("no job running for %s", args.Repo)
		}

		return textResult("cancelling running job for " + args.Repo), nil, nil
	})

	addQueueTools(server, mgr)
}

type bumpTaskArgs struct {
	Seq uint64 `json:"seq" jsonschema:"the task's queue sequence number (seq) as shown by queue_status"`
}

type cancelTaskArgs struct {
	Seq  uint64 `json:"seq,omitempty" jsonschema:"cancel the queued or running task with this sequence number (seq) from queue_status; takes precedence over repo"`
	Repo string `json:"repo,omitempty" jsonschema:"cancel all queued and running tasks for this repo id or web-source key; ignored when seq is set"`
}

type setTaskConcurrencyArgs struct {
	Limit int `json:"limit" jsonschema:"how many background tasks may run at once; values <= 0 restore the built-in default"`
}

// addQueueTools registers the background work-queue management tools. The queue
// funnels all background work (clone/refresh, docs, code_index, reindex,
// web-sync) through one bounded FIFO; these tools let a caller inspect it,
// reprioritize a waiting task, drop unwanted ones and retune concurrency live.
func addQueueTools(server *mcp.Server, mgr *manager.Manager) {
	addTool(server, &mcp.Tool{
		Name: "queue_status",
		Description: "Inspect the background work queue: the concurrency limit, how many tasks are " +
			"running vs pending, and the list of tasks (running, queued and recently finished) with each " +
			"task's seq, id, kind and state. Use a queued task's seq with bump_task, or any live task's seq with cancel_task.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		return jsonResult(mgr.TaskSnapshot()), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "bump_task",
		Description: "Move a queued task to the front of the backlog so it starts next when a slot frees " +
			"(or immediately if one is free). Identify the task by its seq from queue_status. Only queued " +
			"tasks can be bumped; a running or finished task cannot.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args bumpTaskArgs) (*mcp.CallToolResult, any, error) {
		if !mgr.BumpTask(args.Seq) {
			return nil, nil, fmt.Errorf("no queued task with seq %d (it may be running or already finished)", args.Seq)
		}

		return textResult(fmt.Sprintf("bumped task %d to the front of the queue", args.Seq)), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "cancel_task",
		Description: "Cancel background work. Pass seq to cancel one task (from queue_status): a queued task " +
			"is dropped from the backlog and a running task has its job aborted. Or pass repo to cancel every " +
			"queued and running task for a repo id or web-source key (web:<name>). seq takes " +
			"precedence when both are given.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args cancelTaskArgs) (*mcp.CallToolResult, any, error) {
		if args.Seq != 0 {
			if !mgr.CancelTask(args.Seq) {
				return nil, nil, fmt.Errorf("no task with seq %d (it may already be finished)", args.Seq)
			}

			return textResult(fmt.Sprintf("cancelled task %d", args.Seq)), nil, nil
		}

		if args.Repo == "" {
			return nil, nil, fmt.Errorf("provide seq or repo to cancel")
		}

		n := mgr.CancelTasks(args.Repo)

		return textResult(fmt.Sprintf("cancelled %d task(s) for %s", n, args.Repo)), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "set_task_concurrency",
		Description: "Change how many background tasks run concurrently, effective immediately. Raising it " +
			"lets waiting tasks start at once; lowering it takes effect as running tasks finish (it never " +
			"interrupts work in progress). A value <= 0 restores the default. This is the runtime queue " +
			"limit; it is not persisted to settings.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args setTaskConcurrencyArgs) (*mcp.CallToolResult, any, error) {
		mgr.SetTaskConcurrency(args.Limit)

		return jsonResult(mgr.TaskSnapshot()), nil, nil
	})
}

// waitContext bounds a wait=true call so a build that outlives the caller's
// patience still yields an in-progress answer instead of blocking forever.
func waitContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, timeout)
}

// waitResult renders the outcome of a wait=true call. When the build did not
// finish within the wait, a note explains that it keeps running in the
// background and how to follow up.
func waitResult(mgr *manager.Manager, repo *registry.Repo, done bool) *mcp.CallToolResult {
	res := jsonResult(viewRepo(mgr, repo))
	if !done {
		note := &mcp.TextContent{Text: "build still in progress: the wait ended before it finished, " +
			"but it continues in the background; poll repo_status " + repo.ID +
			" until status is 'ready' or 'error'"}
		res.Content = append([]mcp.Content{note}, res.Content...)
	}

	return res
}

// ---- credential tools -------------------------------------------------------

type setCredentialArgs struct {
	Pattern  string `json:"pattern" jsonschema:"host or host/path prefix this credential applies to, e.g. 'gitlab.example.com' or 'github.com/rakunlabs'; the most specific pattern wins"`
	Secret   string `json:"secret" jsonschema:"SSH private key (PEM content) or access token (PAT)"`
	Kind     string `json:"kind,omitempty" jsonschema:"'ssh' for private keys (ssh urls) or 'token' for access tokens (https urls); inferred from the secret when omitted"`
	Username string `json:"username,omitempty" jsonschema:"username for https token auth (default 'oauth2'; GitHub accepts any)"`
}

type credentialPatternArgs struct {
	Pattern string `json:"pattern" jsonschema:"the credential pattern to remove, as shown by list_credentials"`
}

type listCredentialsArgs struct {
	Page    int `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage int `json:"per_page,omitempty" jsonschema:"credentials per page (default 50, max 200)"`
}

func addCredentialTools(server *mcp.Server, mgr *manager.Manager) {
	addTool(server, &mcp.Tool{
		Name: "set_credential",
		Description: "Store a git credential for a host or host/path prefix. Used when cloning/pulling " +
			"matching repositories. Example: pattern 'gitlab.example.com' with an SSH key, or " +
			"pattern 'github.com/myorg' with a token for https clones. The secret is never shown again.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args setCredentialArgs) (*mcp.CallToolResult, any, error) {
		cred := &credentials.Credential{
			Pattern:  args.Pattern,
			Kind:     args.Kind,
			Username: args.Username,
			Secret:   args.Secret,
		}
		if err := mgr.Credentials().Set(ctx, cred); err != nil {
			return nil, nil, err
		}

		return textResult(fmt.Sprintf("stored %s credential for pattern %q", cred.Kind, cred.Pattern)), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "list_credentials",
		Description: "List stored git credential patterns (kind and username only; secrets are never returned).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listCredentialsArgs) (*mcp.CallToolResult, any, error) {
		creds, err := mgr.Credentials().List(ctx)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(pageSlice(creds, args.Page, args.PerPage, 50)), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "remove_credential",
		Description: "Remove a stored git credential by its pattern.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args credentialPatternArgs) (*mcp.CallToolResult, any, error) {
		if err := mgr.Credentials().Delete(ctx, args.Pattern); err != nil {
			return nil, nil, err
		}

		return textResult("removed credential for pattern " + args.Pattern), nil, nil
	})
}

// ---- query tools (proxied to graphify serve) --------------------------------

// repoField documents the shared repo selector on query tools.
const repoField = "repository id (owner/name) to query; always provide it when known, and omit only for explicit cross-repository analysis"

// namespaceField documents the shared namespace selector. When repo is omitted,
// the query is scoped to this namespace; an omitted namespace means the
// 'default' namespace, and '*' searches every namespace.
const namespaceField = "namespace to scope to when repo is omitted; empty means the 'default' namespace, '*' searches all namespaces"

type queryGraphArgs struct {
	Question    string   `json:"question" jsonschema:"architectural or relationship question; use search_code instead for symbols, paths, literals, definitions, and usages"`
	Repo        string   `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Namespace   string   `json:"namespace,omitempty" jsonschema:"namespace to scope to when repo is omitted; empty means the 'default' namespace, '*' searches all namespaces"`
	Mode        string   `json:"mode,omitempty" jsonschema:"traversal mode: 'bfs' for broad context (default) or 'dfs' to trace a specific path"`
	Depth       int      `json:"depth,omitempty" jsonschema:"traversal depth 1-6 (default 3)"`
	TokenBudget int      `json:"token_budget,omitempty" jsonschema:"max output tokens (default 2000, max 4000)"`
	Context     []string `json:"context_filter,omitempty" jsonschema:"optional explicit edge-context filter, e.g. ['call','field']"`
}

type nodeArgs struct {
	Label     string `json:"label" jsonschema:"node label or ID to look up"`
	Repo      string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace to scope to when repo is omitted; empty means the 'default' namespace, '*' searches all namespaces"`
}

type neighborsArgs struct {
	Label          string `json:"label" jsonschema:"node label or ID"`
	RelationFilter string `json:"relation_filter,omitempty" jsonschema:"optional: filter by relation type (e.g. 'calls', 'references', 'method', 'contains'). Direction is shown by arrows in the output (--> successor, <-- predecessor), not by the filter. An unknown relation returns an error listing the node's valid relations; get_node also lists them under 'Relations:'"`
	Repo           string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Namespace      string `json:"namespace,omitempty" jsonschema:"namespace to scope to when repo is omitted; empty means the 'default' namespace, '*' searches all namespaces"`
	Page           int    `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage        int    `json:"per_page,omitempty" jsonschema:"neighbors per page (default 50, max 200)"`
}

type communityArgs struct {
	CommunityID int    `json:"community_id" jsonschema:"community ID (0-indexed by size)"`
	Repo        string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Namespace   string `json:"namespace,omitempty" jsonschema:"namespace to scope to when repo is omitted; empty means the 'default' namespace, '*' searches all namespaces"`
	Page        int    `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage     int    `json:"per_page,omitempty" jsonschema:"nodes per page (default 50, max 200)"`
}

type godNodesArgs struct {
	TopN      int    `json:"top_n,omitempty" jsonschema:"number of nodes to return (default 10, max 50)"`
	Repo      string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace to scope to when repo is omitted; empty means the 'default' namespace, '*' searches all namespaces"`
}

type statsArgs struct {
	Repo      string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace to scope to when repo is omitted; empty means the 'default' namespace, '*' searches all namespaces"`
}

type shortestPathArgs struct {
	Source    string `json:"source" jsonschema:"source concept label or keyword"`
	Target    string `json:"target" jsonschema:"target concept label or keyword"`
	MaxHops   int    `json:"max_hops,omitempty" jsonschema:"maximum hops to consider (default 8, max 12)"`
	Repo      string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace to scope to when repo is omitted; empty means the 'default' namespace, '*' searches all namespaces"`
}

func addQueryTools(server *mcp.Server, mgr *manager.Manager) {
	addTool(server, &mcp.Tool{
		Name:        "query_graph",
		Description: "Answer architecture, dependency, call/data-flow, and cross-file relationship questions by traversing the code knowledge graph. Use search_code instead for symbols, paths, literals, definitions, usages, or implementation locations.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args queryGraphArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{"question": args.Question}
		setIf(call, "mode", args.Mode)
		setIfInt(call, "depth", args.Depth)
		setIfInt(call, "token_budget", args.TokenBudget)

		if len(args.Context) > 0 {
			call["context_filter"] = args.Context
		}

		res, err := mgr.CallGraphTool(ctx, args.Repo, args.Namespace, "query_graph", call)

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "get_node",
		Description: "Get full details for a specific node by label or ID. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args nodeArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.CallGraphTool(ctx, args.Repo, args.Namespace, "get_node", map[string]any{"label": args.Label})

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "get_neighbors",
		Description: "Inspect one bounded page of direct neighbors after query_graph or get_node identifies a target. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args neighborsArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{"label": args.Label}
		setIf(call, "relation_filter", args.RelationFilter)
		setIfInt(call, "page", args.Page)
		setIfInt(call, "per_page", args.PerPage)

		res, err := mgr.CallGraphTool(ctx, args.Repo, args.Namespace, "get_neighbors", call)

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "get_community",
		Description: "Inspect one bounded page of nodes in a known community. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args communityArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{"community_id": args.CommunityID}
		setIfInt(call, "page", args.Page)
		setIfInt(call, "per_page", args.PerPage)
		res, err := mgr.CallGraphTool(ctx, args.Repo, args.Namespace, "get_community", call)

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "god_nodes",
		Description: "Return the most connected nodes - the core abstractions of the codebase. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args godNodesArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{}
		setIfInt(call, "top_n", args.TopN)

		res, err := mgr.CallGraphTool(ctx, args.Repo, args.Namespace, "god_nodes", call)

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "graph_stats",
		Description: "Return graph statistics: node count, edge count, communities, confidence breakdown. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args statsArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.CallGraphTool(ctx, args.Repo, args.Namespace, "graph_stats", map[string]any{})

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "shortest_path",
		Description: "Find the shortest path between two concepts in the knowledge graph. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args shortestPathArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{"source": args.Source, "target": args.Target}
		setIfInt(call, "max_hops", args.MaxHops)

		res, err := mgr.CallGraphTool(ctx, args.Repo, args.Namespace, "shortest_path", call)

		return res, nil, err
	})
}

// ---- file tools (source access from the clone) ------------------------------

type readFileArgs struct {
	Repo     string `json:"repo" jsonschema:"repository id (owner/name) whose clone to read from"`
	Path     string `json:"path" jsonschema:"repo-relative file path, e.g. 'listener/processor.go' (as shown in graph node src fields)"`
	Snapshot string `json:"snapshot,omitempty" jsonschema:"snapshot token returned by an earlier read_file call; pass it on continuation reads to stay on the same commit"`
	Offset   int64  `json:"offset,omitempty" jsonschema:"byte offset to start reading from (default 0); use with the truncated flag to page through large files"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"max bytes to return (default 32768, max 131072)"`
}

type listFilesArgs struct {
	Repo      string `json:"repo" jsonschema:"repository id (owner/name) whose clone to list"`
	Subdir    string `json:"subdir,omitempty" jsonschema:"repo-relative directory to list (default: repository root)"`
	Snapshot  string `json:"snapshot,omitempty" jsonschema:"snapshot token returned by an earlier list_files call; pass it on later pages to keep a stable listing"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"when true, walk the whole subtree (skips .git and graphify-out); otherwise list one level"`
	Page      int    `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage   int    `json:"per_page,omitempty" jsonschema:"entries per page (default 100, max 200)"`
}

type gitBlameArgs struct {
	Repo      string `json:"repo" jsonschema:"repository id (owner/name) whose clone to blame"`
	Path      string `json:"path" jsonschema:"repo-relative file path to blame (as shown in graph node src fields)"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"first line to blame (1-based); omit or <=0 to blame the whole file"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"last line to blame (inclusive); omit or <=0 to blame from start_line to end of file"`
	Snapshot  string `json:"snapshot,omitempty" jsonschema:"snapshot token from an earlier read_file/list_files call; pass it to blame the same commit"`
}

// ---- git history tools ------------------------------------------------------

// The jsonschema strings below are deliberately terse: every tool schema is
// sent on each session's tools/list, and the package budgets that payload
// (see server_test.go). Nuance that only matters once a call is being made
// belongs in the tool Description, not in every field.
//
// None of these take a snapshot token, unlike read_file and git_blame. Those
// address content by position, which shifts between clone versions; history is
// addressed by revision, which does not.
type listRefsArgs struct {
	Repo  string `json:"repo" jsonschema:"repository id (owner/name)"`
	Kind  string `json:"kind,omitempty" jsonschema:"'tag' or 'branch'; omit for both"`
	Limit int    `json:"limit,omitempty" jsonschema:"max refs (default/max 200)"`
}

type gitLogArgs struct {
	Repo  string `json:"repo" jsonschema:"repository id (owner/name)"`
	From  string `json:"from,omitempty" jsonschema:"start revision (tag/branch/sha), exclusive"`
	To    string `json:"to,omitempty" jsonschema:"end revision, inclusive; omit for the branch head"`
	Path  string `json:"path,omitempty" jsonschema:"repo-relative file or directory to follow"`
	Skip  int    `json:"skip,omitempty" jsonschema:"commits to skip, for older pages"`
	Limit int    `json:"limit,omitempty" jsonschema:"max commits (default/max 200)"`
}

type gitDiffArgs struct {
	Repo  string `json:"repo" jsonschema:"repository id (owner/name)"`
	From  string `json:"from,omitempty" jsonschema:"start revision, exclusive; omit to explain the single commit in 'to'"`
	To    string `json:"to" jsonschema:"end revision, or the commit to explain"`
	Path  string `json:"path,omitempty" jsonschema:"repo-relative path to narrow a large change set"`
	Patch bool   `json:"patch,omitempty" jsonschema:"include the change text (off by default; a release-sized patch is large)"`
}

func addHistoryTools(server *mcp.Server, mgr *manager.Manager) {
	addTool(server, &mcp.Tool{
		Name: "list_refs",
		Description: "List a repository's tags and branches, newest first. Call it for exact version names before git_log or git_diff. " +
			"Only the tracked branch is fetched, so revisions must be reachable from it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listRefsArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.RepoRefs(ctx, args.Repo, args.Kind, args.Limit)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(res), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "git_log",
		Description: "Commit history: author, date, full message and touched files. from+to reads what landed between two releases; path follows one file. " +
			"Metadata only - use git_diff for the changes.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args gitLogArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.RepoLog(ctx, args.Repo, gitops.LogOptions{
			From:  args.From,
			To:    args.To,
			Path:  args.Path,
			Skip:  args.Skip,
			Limit: args.Limit,
		})
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(res), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "git_diff",
		Description: "What changed between two revisions, or what one commit changed when from is omitted - this turns a git_blame sha into an explanation. " +
			"Returns the changed files and their status; patch=true adds the change text, path narrows a large change set.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args gitDiffArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.RepoDiff(ctx, args.Repo, args.From, args.To, args.Path, args.Patch)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(res), nil, nil
	})
}

func addFileTools(server *mcp.Server, mgr *manager.Manager) {
	addTool(server, &mcp.Tool{
		Name: "read_file",
		Description: "Read the source of a file inside a tracked repository's clone. " +
			"Use this to see the actual code behind a graph node (node 'src' fields give the path). " +
			"Access is sandboxed to the repo; large files are truncated - page with offset until truncated is false, " +
			"passing the returned snapshot token on continuation reads.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args readFileArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.ReadRepoFileAt(ctx, args.Repo, args.Path, args.Snapshot, args.Offset, mcpReadSize(args.MaxBytes))
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(res), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "list_files",
		Description: "Inspect one known directory, or discover a path when search_code cannot identify it. Pass the returned snapshot token on later pages. Do not request recursive=true unless the user explicitly needs a tree or inventory.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listFilesArgs) (*mcp.CallToolResult, any, error) {
		entries, err := mgr.ListRepoFilesPageAt(ctx, args.Repo, args.Subdir, args.Snapshot, args.Recursive, args.Page, args.PerPage)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(entries), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name: "git_blame",
		Description: "Show git blame for a file inside a tracked repository's clone: who last changed each line, in which commit and when. " +
			"Use it after read_file/search_code locates code to attribute a specific snippet - pass start_line/end_line to blame just that range instead of the whole file. " +
			"Pass the snapshot token from an earlier read to attribute the same commit.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args gitBlameArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.BlameRepoFile(ctx, args.Repo, args.Path, args.Snapshot, args.StartLine, args.EndLine)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(res), nil, nil
	})
}

// ---- helpers ----------------------------------------------------------------

func setIf(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}

func setIfInt(m map[string]any, key string, val int) {
	if val != 0 {
		m[key] = val
	}
}

func mcpReadSize(size int) int {
	if size <= 0 {
		return 32 * 1024
	}
	if size > 128*1024 {
		return 128 * 1024
	}
	return size
}

func boundedCount(value, defaultValue, maxValue int) int {
	if value <= 0 {
		return defaultValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

type pageResult[T any] struct {
	Items   []T  `json:"items"`
	Total   int  `json:"total"`
	Page    int  `json:"page"`
	PerPage int  `json:"per_page"`
	HasMore bool `json:"has_more"`
}

func pageSlice[T any](items []T, page, perPage, defaultPerPage int) pageResult[T] {
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if perPage > 200 {
		perPage = 200
	}

	offset := len(items)
	if page-1 <= len(items)/perPage {
		offset = (page - 1) * perPage
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + perPage
	if end > len(items) {
		end = len(items)
	}

	return pageResult[T]{
		Items: items[offset:end], Total: len(items), Page: page,
		PerPage: perPage, HasMore: end < len(items),
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return textResult(fmt.Sprintf("marshal error: %v", err))
	}

	return textResult(string(b))
}

// spec converts the add_repo arguments into the manager's registration spec.
func (a addRepoArgs) spec() manager.RepoSpec {
	return manager.RepoSpec{
		URL:       a.URL,
		Branch:    a.Branch,
		Namespace: a.Namespace,
		Overrides: a.overrides(),
	}
}
