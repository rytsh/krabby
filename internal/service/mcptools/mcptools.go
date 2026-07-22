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

// New builds the MCP server with all krabby tools registered. waitTimeout caps
// how long wait=true management calls block before returning the in-progress
// status (the build keeps running in the background); <=0 means no server-side
// cap.
func New(mgr *manager.Manager, version string, waitTimeout time.Duration) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "krabby",
		Title:   "krabby - multi-repo graphify knowledge graphs",
		Version: version,
	}, nil)

	addManagementTools(server, mgr, waitTimeout)
	addLeaseTools(server, mgr)
	addCredentialTools(server, mgr)
	addQueryTools(server, mgr)
	addFileTools(server, mgr)
	addDocTools(server, mgr)

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
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_repos",
		Description: "List all tracked repositories with build status, currently running pipeline step, last commit and last build time.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		repos, err := mgr.Registry().List(ctx)
		if err != nil {
			return nil, nil, err
		}

		views := make([]repoView, 0, len(repos))
		for _, repo := range repos {
			views = append(views, viewRepo(mgr, repo))
		}

		return jsonResult(views), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
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

	mcp.AddTool(server, &mcp.Tool{
		Name:        "remove_repo",
		Description: "Stop tracking a repository and delete its local clone and graph.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args repoIDArgs) (*mcp.CallToolResult, any, error) {
		if err := mgr.RemoveRepo(ctx, args.Repo); err != nil {
			return nil, nil, err
		}

		return textResult("removed " + args.Repo), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
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
				mgr.TriggerGenerate(args.Repo, args.Stages)

				return textResult(fmt.Sprintf("generate %v queued for %s", args.Stages, args.Repo)), nil, nil
			}

			wctx, cancel := waitContext(ctx, waitTimeout)
			defer cancel()

			repo, done, err := mgr.GenerateWait(wctx, args.Repo, args.Stages)
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

	mcp.AddTool(server, &mcp.Tool{
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

	mcp.AddTool(server, &mcp.Tool{
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
	mcp.AddTool(server, &mcp.Tool{
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

	mcp.AddTool(server, &mcp.Tool{
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

func addCredentialTools(server *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(server, &mcp.Tool{
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

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_credentials",
		Description: "List stored git credential patterns (kind and username only; secrets are never returned).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		creds, err := mgr.Credentials().List(ctx)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(creds), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
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
const repoField = "repository id (owner/name) to query; omit to query the merged cross-repo graph (must be enabled via graphify.merge)"

type queryGraphArgs struct {
	Question    string   `json:"question" jsonschema:"natural language question or keyword search"`
	Repo        string   `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; omit to query the merged cross-repo graph (must be enabled via graphify.merge)"`
	Mode        string   `json:"mode,omitempty" jsonschema:"traversal mode: 'bfs' for broad context (default) or 'dfs' to trace a specific path"`
	Depth       int      `json:"depth,omitempty" jsonschema:"traversal depth 1-6 (default 3)"`
	TokenBudget int      `json:"token_budget,omitempty" jsonschema:"max output tokens (default 2000)"`
	Context     []string `json:"context_filter,omitempty" jsonschema:"optional explicit edge-context filter, e.g. ['call','field']"`
}

type nodeArgs struct {
	Label string `json:"label" jsonschema:"node label or ID to look up"`
	Repo  string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; omit to query the merged cross-repo graph (must be enabled via graphify.merge)"`
}

type neighborsArgs struct {
	Label          string `json:"label" jsonschema:"node label or ID"`
	RelationFilter string `json:"relation_filter,omitempty" jsonschema:"optional: filter by relation type"`
	Repo           string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; omit to query the merged cross-repo graph (must be enabled via graphify.merge)"`
}

type communityArgs struct {
	CommunityID int    `json:"community_id" jsonschema:"community ID (0-indexed by size)"`
	Repo        string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; omit to query the merged cross-repo graph (must be enabled via graphify.merge)"`
}

type godNodesArgs struct {
	TopN int    `json:"top_n,omitempty" jsonschema:"number of nodes to return (default 10)"`
	Repo string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; omit to query the merged cross-repo graph (must be enabled via graphify.merge)"`
}

type statsArgs struct {
	Repo string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; omit to query the merged cross-repo graph (must be enabled via graphify.merge)"`
}

type shortestPathArgs struct {
	Source  string `json:"source" jsonschema:"source concept label or keyword"`
	Target  string `json:"target" jsonschema:"target concept label or keyword"`
	MaxHops int    `json:"max_hops,omitempty" jsonschema:"maximum hops to consider (default 8)"`
	Repo    string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to query; omit to query the merged cross-repo graph (must be enabled via graphify.merge)"`
}

func addQueryTools(server *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "query_graph",
		Description: "Search the code knowledge graph of one repo (or all repos merged) using BFS or DFS. " +
			"Returns relevant nodes and edges as text context. Best first call for any codebase question.",
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

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_node",
		Description: "Get full details for a specific node by label or ID. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args nodeArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.CallGraphTool(ctx, args.Repo, "get_node", map[string]any{"label": args.Label})

		return res, nil, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_neighbors",
		Description: "Get all direct neighbors of a node with edge details. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args neighborsArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{"label": args.Label}
		setIf(call, "relation_filter", args.RelationFilter)

		res, err := mgr.CallGraphTool(ctx, args.Repo, "get_neighbors", call)

		return res, nil, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_community",
		Description: "Get all nodes in a community by community ID. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args communityArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.CallGraphTool(ctx, args.Repo, "get_community", map[string]any{"community_id": args.CommunityID})

		return res, nil, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "god_nodes",
		Description: "Return the most connected nodes - the core abstractions of the codebase. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args godNodesArgs) (*mcp.CallToolResult, any, error) {
		call := map[string]any{}
		setIfInt(call, "top_n", args.TopN)

		res, err := mgr.CallGraphTool(ctx, args.Repo, "god_nodes", call)

		return res, nil, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "graph_stats",
		Description: "Return graph statistics: node count, edge count, communities, confidence breakdown. " + repoField + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args statsArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.CallGraphTool(ctx, args.Repo, "graph_stats", map[string]any{})

		return res, nil, err
	})

	mcp.AddTool(server, &mcp.Tool{
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
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"max bytes to return in this call (default and cap 524288)"`
}

type listFilesArgs struct {
	Repo      string `json:"repo" jsonschema:"repository id (owner/name) whose clone to list"`
	Subdir    string `json:"subdir,omitempty" jsonschema:"repo-relative directory to list (default: repository root)"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"when true, walk the whole subtree (skips .git and graphify-out); otherwise list one level"`
}

func addFileTools(server *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "read_file",
		Description: "Read the source of a file inside a tracked repository's clone. " +
			"Use this to see the actual code behind a graph node (node 'src' fields give the path). " +
			"Access is sandboxed to the repo; large files are truncated - page with offset until truncated is false.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args readFileArgs) (*mcp.CallToolResult, any, error) {
		res, err := mgr.ReadRepoFile(ctx, args.Repo, args.Path, args.Offset, args.MaxBytes)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(res), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_files",
		Description: "List files and directories inside a tracked repository's clone. " +
			"Use to explore layout before reading files. Set recursive=true for the full tree.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listFilesArgs) (*mcp.CallToolResult, any, error) {
		entries, err := mgr.ListRepoFiles(ctx, args.Repo, args.Subdir, args.Recursive)
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

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return textResult(fmt.Sprintf("marshal error: %v", err))
	}

	return textResult(string(b))
}
