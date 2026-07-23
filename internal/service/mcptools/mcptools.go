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
	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/registry"
)

// serverInstructions is the server-level guidance returned to clients on
// initialize. Most MCP clients surface it to the LLM as high-level context, so
// it explains what krabby is, the add->poll->query lifecycle, and which tool to
// reach for first. Per-tool specifics stay in each tool's Description.
const serverInstructions = `Krabby indexes tracked git repositories for source search, documentation retrieval, and code-relationship analysis.

Tool selection:
- Use search_code first for symbols, paths, literals, definitions, usages, and implementation locations. Use normal mode for exact text and semantic mode for conceptual source search.
- Use query_graph for architecture, dependencies, call/data flow, and relationships across files. It is not a keyword or symbol search.
- Use search_docs for documentation, guides, wikis, and Confluence content.
- Use list_* only when an identifier is unknown or the user explicitly requests an inventory. Do not exhaust pages or request a recursive file tree without a clear need.
- Use get_* tools only after a search/query identifies the target.
- If a graph tool returns "Repository selection required", retry it with one of the provided repo ids instead of treating the result as a failure.

Always pass repo when it is known. Omit repo only when the user explicitly requests cross-repository analysis and merged search is intended.

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
		instructions += "\n\nThis connection uses the full profile and can mutate credentials and runtime configuration. Use administration tools only when explicitly requested."
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "krabby",
		Title:   title,
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	addManagementTools(server, mgr, waitTimeout)
	addQueryTools(server, mgr)
	addFileTools(server, mgr)
	addDocTools(server, mgr, profile == ToolProfileFull)
	if profile == ToolProfileFull {
		addLeaseTools(server, mgr)
		addCredentialTools(server, mgr)
	}

	return server
}

// ---- management tools -------------------------------------------------------

type addRepoArgs struct {
	URL    string `json:"url" jsonschema:"git URL of the repository (ssh or https)"`
	Branch string `json:"branch,omitempty" jsonschema:"branch to track (default: repo default branch)"`
	Wait   bool   `json:"wait,omitempty" jsonschema:"when true, block until the clone and graph build finish and return the final status (ready or error) instead of returning immediately"`
}

type repoIDArgs struct {
	Repo string `json:"repo" jsonschema:"repository id in owner/name form"`
}

type refreshRepoArgs struct {
	Repo   string   `json:"repo" jsonschema:"repository id in owner/name form"`
	Wait   bool     `json:"wait,omitempty" jsonschema:"when true, block until the pull and graph rebuild finish and return the final status (ready or error) instead of returning immediately"`
	Stages []string `json:"stages,omitempty" jsonschema:"optional subset of pipeline stages to rebuild against the existing clone without pulling git: graph, docs, docs_index, code_index. Empty runs the full pull+rebuild pipeline. Use e.g. ['docs_index'] to re-embed docs after they were regenerated. Missing prerequisites (docs_index needs docs, which needs graph) are built automatically only when their output is absent"`
	Force  bool     `json:"force,omitempty" jsonschema:"when true, the docs stage ignores its incremental caches and regenerates every per-file summary and documentation.md even if nothing changed. Requires stages to include 'docs' (otherwise docs are reused because unchanged). Ignored by the full pull+rebuild pipeline (empty stages)"`
}

// validateStages rejects unknown stage names so a typo fails fast with a clear
// message instead of silently doing nothing. An empty stages list is valid and
// selects the full pull+rebuild pipeline.
func (a refreshRepoArgs) validateStages() error {
	for _, s := range a.Stages {
		if !registry.ValidStage(s) {
			return fmt.Errorf("unknown stage %q; valid stages are: %s, %s, %s, %s",
				s, registry.StageGraph, registry.StageDocs, registry.StageDocsIndex, registry.StageCodeIndex)
		}
	}

	return nil
}

type emptyArgs struct{}

type listReposArgs struct {
	Page    int    `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage int    `json:"per_page,omitempty" jsonschema:"results per page (default 20, max 200)"`
	Search  string `json:"search,omitempty" jsonschema:"case-insensitive substring filter on the repo id (host/group/.../name)"`
	Owner   string `json:"owner,omitempty" jsonschema:"restrict to the direct children of one directory prefix (everything before the repo name)"`
}

// repoView decorates a repo record with the transient in-memory activity so
// callers can see which pipeline step is currently running (empty = idle).
type repoView struct {
	*registry.Repo
	Running string `json:"running,omitempty"`
}

func viewRepo(mgr *manager.Manager, repo *registry.Repo) repoView {
	return repoView{Repo: repo, Running: mgr.Activity(repo.ID)}
}

func addManagementTools(server *mcp.Server, mgr *manager.Manager, waitTimeout time.Duration) {
	addTool(server, &mcp.Tool{
		Name:        "list_repos",
		Description: "Discover tracked repository ids and build status when the target repo is unknown, or when the user asks for an inventory. Filter with search/owner and inspect one page; do not fetch every page routinely.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listReposArgs) (*mcp.CallToolResult, any, error) {
		opts := registry.ListOptions{
			Page:    args.Page,
			PerPage: args.PerPage,
			Search:  args.Search,
			Owner:   args.Owner,
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
			repo, err := mgr.AddRepo(ctx, args.URL, args.Branch)
			if err != nil {
				return nil, nil, err
			}

			return jsonResult(repo), nil, nil
		}

		wctx, cancel := waitContext(ctx, waitTimeout)
		defer cancel()

		repo, done, err := mgr.AddRepoWait(wctx, args.URL, args.Branch)
		if err != nil {
			return nil, nil, err
		}

		return waitResult(mgr, repo, done), nil, nil
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
			"Pass wait=true to wait for the result: it returns the final status when the rebuild finishes in time, " +
			"otherwise the in-progress status. The rebuild always continues in the background even if the call " +
			"times out or is cancelled; poll repo_status until status is 'ready' or 'error'. " +
			"Use when you know the repo changed. " +
			"Pass stages to rebuild only a subset of pipeline stages (graph, docs, docs_index, code_index) " +
			"against the existing clone WITHOUT pulling git; empty stages runs the full pull+rebuild pipeline.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args refreshRepoArgs) (*mcp.CallToolResult, any, error) {
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
			mgr.TriggerRefresh(args.Repo)

			return textResult("refresh queued for " + args.Repo), nil, nil
		}

		wctx, cancel := waitContext(ctx, waitTimeout)
		defer cancel()

		repo, done, err := mgr.RefreshWait(wctx, args.Repo)
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

// ---- lease tools ------------------------------------------------------------

type lockRepoArgs struct {
	Repo  string `json:"repo" jsonschema:"repository id in owner/name form"`
	Owner string `json:"owner,omitempty" jsonschema:"who holds the lock, e.g. 'docgen' (informational)"`
	TTL   string `json:"ttl,omitempty" jsonschema:"lock duration as Go duration, e.g. '5m' (default 10m, max 1h)"`
}

type unlockRepoArgs struct {
	Repo  string `json:"repo" jsonschema:"repository id in owner/name form"`
	Token string `json:"token" jsonschema:"the token returned by lock_repo"`
}

func addLeaseTools(server *mcp.Server, mgr *manager.Manager) {
	addTool(server, &mcp.Tool{
		Name: "lock_repo",
		Description: "Take a TTL-bounded read lock on a repository clone so external tools can walk it " +
			"without a refresh pulling mid-read. Deferred refreshes run automatically on unlock/expiry. " +
			"Returns a token required for unlock_repo.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args lockRepoArgs) (*mcp.CallToolResult, any, error) {
		var ttl time.Duration

		if args.TTL != "" {
			d, err := time.ParseDuration(args.TTL)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid ttl; %w", err)
			}

			ttl = d
		}

		l, err := mgr.AcquireLease(ctx, args.Repo, args.Owner, ttl)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(l), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "unlock_repo",
		Description: "Release a read lock taken with lock_repo. Any refresh deferred during the lock runs immediately.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args unlockRepoArgs) (*mcp.CallToolResult, any, error) {
		if err := mgr.ReleaseLease(args.Repo, args.Token); err != nil {
			return nil, nil, err
		}

		return textResult("unlocked " + args.Repo), nil, nil
	})
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

type queryGraphArgs struct {
	Question    string   `json:"question" jsonschema:"architectural or relationship question; use search_code instead for symbols, paths, literals, definitions, and usages"`
	Repo        string   `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Mode        string   `json:"mode,omitempty" jsonschema:"traversal mode: 'bfs' for broad context (default) or 'dfs' to trace a specific path"`
	Depth       int      `json:"depth,omitempty" jsonschema:"traversal depth 1-6 (default 3)"`
	TokenBudget int      `json:"token_budget,omitempty" jsonschema:"max output tokens (default 2000, max 4000)"`
	Context     []string `json:"context_filter,omitempty" jsonschema:"optional explicit edge-context filter, e.g. ['call','field']"`
}

type nodeArgs struct {
	Label string `json:"label" jsonschema:"node label or ID to look up"`
	Repo  string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
}

type neighborsArgs struct {
	Label          string `json:"label" jsonschema:"node label or ID"`
	RelationFilter string `json:"relation_filter,omitempty" jsonschema:"optional: filter by relation type (e.g. 'calls', 'references', 'method', 'contains'). Direction is shown by arrows in the output (--> successor, <-- predecessor), not by the filter. An unknown relation returns an error listing the node's valid relations; get_node also lists them under 'Relations:'"`
	Repo           string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Page           int    `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage        int    `json:"per_page,omitempty" jsonschema:"neighbors per page (default 50, max 200)"`
}

type communityArgs struct {
	CommunityID int    `json:"community_id" jsonschema:"community ID (0-indexed by size)"`
	Repo        string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
	Page        int    `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage     int    `json:"per_page,omitempty" jsonschema:"nodes per page (default 50, max 200)"`
}

type godNodesArgs struct {
	TopN int    `json:"top_n,omitempty" jsonschema:"number of nodes to return (default 10, max 50)"`
	Repo string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
}

type statsArgs struct {
	Repo string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
}

type shortestPathArgs struct {
	Source  string `json:"source" jsonschema:"source concept label or keyword"`
	Target  string `json:"target" jsonschema:"target concept label or keyword"`
	MaxHops int    `json:"max_hops,omitempty" jsonschema:"maximum hops to consider (default 8, max 12)"`
	Repo    string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; always provide when known, omit only for explicit cross-repository analysis"`
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

		res, err := mgr.CallGraphTool(ctx, args.Repo, "query_graph", call)

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "get_node",
		Description: "Get full details for a specific node by label or ID. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args nodeArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.CallGraphTool(ctx, args.Repo, "get_node", map[string]any{"label": args.Label})

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

		res, err := mgr.CallGraphTool(ctx, args.Repo, "get_neighbors", call)

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "get_community",
		Description: "Inspect one bounded page of nodes in a known community. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args communityArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{"community_id": args.CommunityID}
		setIfInt(call, "page", args.Page)
		setIfInt(call, "per_page", args.PerPage)
		res, err := mgr.CallGraphTool(ctx, args.Repo, "get_community", call)

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "god_nodes",
		Description: "Return the most connected nodes - the core abstractions of the codebase. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args godNodesArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{}
		setIfInt(call, "top_n", args.TopN)

		res, err := mgr.CallGraphTool(ctx, args.Repo, "god_nodes", call)

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "graph_stats",
		Description: "Return graph statistics: node count, edge count, communities, confidence breakdown. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args statsArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.CallGraphTool(ctx, args.Repo, "graph_stats", map[string]any{})

		return res, nil, err
	})

	addTool(server, &mcp.Tool{
		Name:        "shortest_path",
		Description: "Find the shortest path between two concepts in the knowledge graph. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args shortestPathArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{"source": args.Source, "target": args.Target}
		setIfInt(call, "max_hops", args.MaxHops)

		res, err := mgr.CallGraphTool(ctx, args.Repo, "shortest_path", call)

		return res, nil, err
	})
}

// ---- file tools (source access from the clone) ------------------------------

type readFileArgs struct {
	Repo     string `json:"repo" jsonschema:"repository id (owner/name) whose clone to read from"`
	Path     string `json:"path" jsonschema:"repo-relative file path, e.g. 'listener/processor.go' (as shown in graph node src fields)"`
	Offset   int64  `json:"offset,omitempty" jsonschema:"byte offset to start reading from (default 0); use with the truncated flag to page through large files"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"max bytes to return (default 32768, max 131072)"`
}

type listFilesArgs struct {
	Repo      string `json:"repo" jsonschema:"repository id (owner/name) whose clone to list"`
	Subdir    string `json:"subdir,omitempty" jsonschema:"repo-relative directory to list (default: repository root)"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"when true, walk the whole subtree (skips .git and graphify-out); otherwise list one level"`
	Page      int    `json:"page,omitempty" jsonschema:"page number (default 1)"`
	PerPage   int    `json:"per_page,omitempty" jsonschema:"entries per page (default 100, max 200)"`
}

func addFileTools(server *mcp.Server, mgr *manager.Manager) {
	addTool(server, &mcp.Tool{
		Name: "read_file",
		Description: "Read the source of a file inside a tracked repository's clone. " +
			"Use this to see the actual code behind a graph node (node 'src' fields give the path). " +
			"Access is sandboxed to the repo; large files are truncated - page with offset until truncated is false.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args readFileArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.ReadRepoFile(ctx, args.Repo, args.Path, args.Offset, mcpReadSize(args.MaxBytes))
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(res), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "list_files",
		Description: "Inspect one known directory, or discover a path when search_code cannot identify it. Do not request recursive=true unless the user explicitly needs a tree or inventory.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listFilesArgs) (*mcp.CallToolResult, any, error) {
		entries, err := mgr.ListRepoFilesPage(ctx, args.Repo, args.Subdir, args.Recursive, args.Page, args.PerPage)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(entries), nil, nil
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
