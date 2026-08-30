<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";

  let settings = $state(null);

  let basePath = $derived((settings && settings.server && settings.server.base_path) || "");
  let mcpPath = $derived((settings && settings.mcp && settings.mcp.path) || "/mcp");
  let apiKeySet = $derived(!!(settings && settings.mcp && settings.mcp.api_key_set));
  let mcpRoot = $derived(`${window.location.origin}${basePath}${mcpPath}`);
  let apiBase = $derived(`${window.location.origin}${basePath}/api/v1`);
  let mcpCatalog = $state("core");
  let mcpUrl = $derived(`${mcpRoot}${mcpCatalog === "core" ? "" : `/${mcpCatalog}`}`);
  let mcpName = $derived(`krabby${mcpCatalog === "core" ? "" : `-${mcpCatalog}`}`);
  let configHeaders = $derived.by(() => {
    const headers = [];
    if (apiKeySet) headers.push(`"X-Api-Key": "<your-api-key>"`);
    return headers.length ? `,\n      "headers": { ${headers.join(", ")} }` : "";
  });
  let cliHeaders = $derived(apiKeySet ? ' --header "X-Api-Key: <your-api-key>"' : "");

  let copied = $state("");
  async function copy(text, key) {
    try {
      await navigator.clipboard.writeText(text);
      copied = key;
      setTimeout(() => (copied = ""), 1500);
    } catch {
      /* clipboard unavailable (http origin); ignore */
    }
  }

  let opencodeConfig = $derived(`{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "${mcpName}": {
      "type": "remote",
      "url": "${mcpUrl}"${configHeaders}
    }
  }
}`);

  let claudeCmd = $derived(`claude mcp add --transport http ${mcpName} ${mcpUrl}${cliHeaders}`);

  let genericConfig = $derived(`{
  "mcpServers": {
    "${mcpName}": {
      "type": "http",
      "url": "${mcpUrl}"${configHeaders}
    }
  }
}`);

  let installPrompt = $derived(`Connect this AI client to the Krabby remote MCP server.

Server name: ${mcpName}
Transport: streamable HTTP
URL: ${mcpUrl}
Tool catalog: ${mcpCatalog} (${mcpCatalog === "core" ? "read-only code, graph, files and docs" : mcpCatalog === "api" ? "API discovery and live endpoint calls" : "Krabby administration and mutations"})
${apiKeySet ? "Authentication: send the API key in the X-Api-Key header. Ask me for the key before editing the configuration." : "Authentication: none"}

Detect this client's MCP configuration format and update the appropriate project or user configuration. Preserve all existing settings and other MCP servers. After configuring it, verify the connection and confirm that the Krabby tools are available.`);

  const triggerRepoUrl = "https://github.com/owner/repo";
  let triggerPrompt = $derived(`Using the Krabby MCP tools, track this repository: ${triggerRepoUrl}

If it is already tracked, refresh it to pull the latest commits and rebuild its knowledge graph. Otherwise add it. Then report the final build status.

The URL can be HTTPS or SSH (e.g. git@github.com:owner/repo.git). For private repos make sure a matching git credential is stored first — a token for HTTPS or an SSH key for SSH URLs.`);

  let curlAddRepo = $derived(`curl -X POST ${apiBase}/repos \\
  -H "Content-Type: application/json" \\
  -d '{"url": "https://github.com/owner/repo", "branch": ""}'`);

  // The complete tool inventory, kept in sync with the registrations in
  // internal/service/mcptools (TestToolCatalogs pins the disjoint counts: 22
  // core, 5 api, 38 admin). A tool's third element selects its catalog;
  // absent means core.
  const toolGroups = [
    {
      name: "Repositories",
      tools: [
        ["list_repos", "List tracked repositories with build status, last commit and last build time."],
        ["add_repo", "Track a new repository: clones it and builds its knowledge graph.", "admin"],
        ["remove_repo", "Stop tracking a repository and delete its local clone and graph.", "admin"],
        ["refresh_repo", "Pull the latest commits and rebuild the knowledge graph.", "admin"],
        ["repo_status", "Get build state, last commit and last error of a repository."],
        ["cancel_repo_job", "Cancel the refresh or generation job currently running for a repository.", "admin"],
        ["set_repo_namespace", "Move a repository into a namespace.", "admin"],
        ["set_repo_overrides", "Set per-repository indexing overrides (include/exclude patterns).", "admin"],
        ["list_refs", "List a repository's branches and tags."],
      ],
    },
    {
      name: "Namespaces",
      tools: [
        ["list_namespaces", "Discover repository groups, counts, and descriptions before broad search."],
        ["set_namespace_description", "Describe a namespace so a model can pick the right group.", "admin"],
        ["delete_namespace", "Delete a namespace description; repositories keep their tag.", "admin"],
      ],
    },
    {
      name: "Knowledge graph",
      tools: [
        ["query_graph", "Analyze architecture, dependencies, flows, and cross-file relationships with BFS/DFS."],
        ["get_node", "Get full details for a node by label or ID."],
        ["get_neighbors", "Page through direct neighbors of a node with edge details."],
        ["get_community", "Page through nodes in a community by community ID."],
        ["god_nodes", "The most connected nodes — the core abstractions of the codebase."],
        ["graph_stats", "Node count, edge count, communities, confidence breakdown."],
        ["shortest_path", "Find the shortest path between two concepts in the graph."],
      ],
    },
    {
      name: "Files & history",
      tools: [
        ["list_files", "Inspect a bounded page of files and directories in a tracked clone."],
        ["read_file", "Read a bounded page of a known source file."],
        ["git_log", "Commit history of a repository, a ref range, or one file."],
        ["git_diff", "Diff between two refs or commits."],
        ["git_blame", "Line-by-line authorship of a file region."],
      ],
    },
    {
      name: "Docs & search",
      tools: [
        ["search_code", "First choice for symbols, paths, definitions, usages, and implementation locations."],
        ["search_docs", "Search repo docs and web sources with hybrid, semantic, or lexical retrieval."],
        ["list_docs", "Page through generated documentation metadata for a repository."],
        ["get_doc", "Read a bounded page of a known generated document."],
        ["list_sources", "List Custom web, Confluence, and Jira collections and their web:<name> search keys."],
        ["get_source", "Inspect one web collection with a bounded sample of its items."],
      ],
    },
    {
      name: "API catalog",
      tools: [
        ["list_api_groups", "List API groups with descriptions — the entry point for 'how do I call this'.", "api"],
        ["list_api_services", "List catalogued API services with title, base URL and endpoint count.", "api"],
        ["list_api_endpoints", "Page through one service's endpoints, narrowed by search, tag, or method.", "api"],
        ["get_api_endpoint", "Full detail of one endpoint: parameters, schemas, auth, and a ready-to-run command.", "api"],
        ["call_api_endpoint", "Send a real request to a catalogued endpoint and return the response.", "api"],
        ["api_service_kinds", "List the service kinds add_api_service accepts.", "admin"],
        ["add_api_service", "Catalogue an OpenAPI document or gRPC server and index its endpoints.", "admin"],
        ["update_api_service", "Update a catalogued service's config, overrides, or schedule.", "admin"],
        ["delete_api_service", "Remove a service and its indexed endpoints.", "admin"],
        ["refresh_api_service", "Queue a re-sync of a catalogued service.", "admin"],
        ["get_api_service_config", "Inspect a service's redacted provider config and overrides.", "admin"],
        ["set_api_group_description", "Create or describe an API group.", "admin"],
        ["delete_api_group", "Delete an API group's description; services keep their tag.", "admin"],
      ],
    },
    {
      name: "Queue",
      tools: [
        ["queue_status", "Inspect the background work queue and running tasks.", "admin"],
        ["bump_task", "Move a queued task to the front.", "admin"],
        ["cancel_task", "Cancel a queued or running task.", "admin"],
        ["set_task_concurrency", "Adjust how many background tasks run in parallel.", "admin"],
      ],
    },
    {
      name: "Web sources",
      tools: [
        ["source_types", "List the web source kinds add_source accepts.", "admin"],
        ["add_source", "Add a Custom web, Confluence, or Jira collection.", "admin"],
        ["update_source", "Update a collection's config or schedule.", "admin"],
        ["delete_source", "Remove a collection and its indexed items.", "admin"],
        ["refresh_source", "Queue a re-sync of a collection.", "admin"],
        ["get_source_config", "Inspect a collection's redacted config.", "admin"],
        ["register_source_page", "Register one page URL in a pages collection.", "admin"],
        ["import_source_pages", "Bulk-register page URLs into a pages collection.", "admin"],
        ["import_source_sitemap", "Import page URLs from a sitemap.", "admin"],
        ["delete_source_page", "Remove one page from a pages collection.", "admin"],
      ],
    },
    {
      name: "Configuration",
      tools: [
        ["get_docs_config", "Return the current docs/RAG configuration (secrets redacted).", "admin"],
        ["set_docs_config", "Update the docs/RAG configuration and rebuild the clients live.", "admin"],
        ["test_llm", "Test the chat LLM connection without saving.", "admin"],
        ["test_embedder", "Test the embeddings connection without saving.", "admin"],
        ["test_code_embedder", "Test the dedicated code embeddings connection without saving.", "admin"],
      ],
    },
    {
      name: "Credentials",
      tools: [
        ["set_credential", "Store a git credential (SSH key or token) for a host or host/path prefix.", "admin"],
        ["list_credentials", "List stored git credential patterns (secrets never returned).", "admin"],
        ["remove_credential", "Remove a stored git credential by its pattern.", "admin"],
      ],
    },
  ];
  let visibleToolGroups = $derived(
    toolGroups
      .map((group) => ({
        ...group,
        tools: group.tools.filter(([, , catalog]) => (catalog || "core") === mcpCatalog),
      }))
      .filter((group) => group.tools.length > 0),
  );

  onMount(async () => {
    try {
      settings = await api.settings();
    } catch {
      settings = null;
    }
  });
</script>

<p class="max-w-[720px] text-dim">
  krabby tracks git repositories, builds a knowledge graph for each one and generates docs and semantic
  indexes on top. Its tools are available to AI agents through three independent
  <span class="text-fg">Model Context Protocol</span> catalogs, split by capability so each client sees
  only what it needs.
</p>

<div class="card my-4 p-4">
  <div class="mb-2 flex items-center gap-2">
    <h2 class="text-[15px] font-semibold">MCP endpoints</h2>
    <span class="rounded border border-line px-1.5 py-0.5 text-[11px] text-faint">streamable HTTP</span>
  </div>
  <p class="mt-0 text-[13px] text-faint">
    Add only the catalogs this client needs. Each is a separate MCP server, so API access and administration can be enabled or disabled independently.
  </p>
  <div class="mt-3 grid gap-2 sm:grid-cols-3">
    <button
      class={`rounded-md border p-3 text-left transition-colors hover:border-accent/60 ${mcpCatalog === "core" ? "border-accent bg-accent/5" : "border-line"}`}
      aria-pressed={mcpCatalog === "core"}
      onclick={() => (mcpCatalog = "core")}
    >
      <div class="text-[13px] font-medium">Core</div>
      <code class="mt-1 block break-all font-mono text-[11px] text-fg">{mcpRoot}</code>
      <div class="mt-1 text-[12px] text-dim">Read-only repository, graph, file, history, and documentation tools.</div>
      <div class="mt-2 text-[11px] font-medium text-accent">22 tools · view below</div>
    </button>
    <button
      class={`rounded-md border p-3 text-left transition-colors hover:border-accent/60 ${mcpCatalog === "api" ? "border-accent bg-accent/5" : "border-line"}`}
      aria-pressed={mcpCatalog === "api"}
      onclick={() => (mcpCatalog = "api")}
    >
      <div class="text-[13px] font-medium">API</div>
      <code class="mt-1 block break-all font-mono text-[11px] text-fg">{mcpRoot}/api</code>
      <div class="mt-1 text-[12px] text-dim">API catalog discovery plus <code class="font-mono">call_api_endpoint</code>, which sends real requests.</div>
      <div class="mt-2 text-[11px] font-medium text-accent">5 tools · view below</div>
    </button>
    <button
      class={`rounded-md border p-3 text-left transition-colors hover:border-accent/60 ${mcpCatalog === "admin" ? "border-accent bg-accent/5" : "border-line"}`}
      aria-pressed={mcpCatalog === "admin"}
      onclick={() => (mcpCatalog = "admin")}
    >
      <div class="text-[13px] font-medium">Admin</div>
      <code class="mt-1 block break-all font-mono text-[11px] text-fg">{mcpRoot}/admin</code>
      <div class="mt-1 text-[12px] text-dim">All create, update, refresh, cancel, delete, credential, queue, and configuration tools.</div>
      <div class="mt-2 text-[11px] font-medium text-accent">38 tools · view below</div>
    </button>
  </div>
  {#if apiKeySet}
    <p class="mb-0 mt-2 text-[13px] text-warn">
      An API key is configured: clients must send it in the <code class="font-mono">X-Api-Key</code> header.
    </p>
  {:else}
    <p class="mb-0 mt-2 text-[13px] text-faint">
      No API key configured — all MCP endpoints are open. Set <code class="font-mono">KRABBY_MCP_API_KEY</code> to protect them.
    </p>
  {/if}
</div>

<div class="card my-4 p-4">
  <h2 class="mb-3 text-[15px] font-semibold">Connect a client</h2>

  <div class="mb-5 rounded-md border border-accent/40 bg-accent/5 p-3">
    <div class="mb-1.5 flex items-center gap-2">
      <div>
        <div class="text-[13px] font-medium">AI-assisted setup prompt</div>
        <div class="text-[11px] text-faint">Paste this into your LLM and let it configure the current client.</div>
      </div>
      <button class="btn btn-sm ml-auto" onclick={() => copy(installPrompt, "prompt")}>
        {copied === "prompt" ? "Copied" : "Copy"}
      </button>
    </div>
    <pre class="m-0 max-h-64 overflow-auto whitespace-pre-wrap rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{installPrompt}</pre>
  </div>

  <div class="mb-4">
    <div class="mb-1.5 flex flex-wrap items-center gap-2">
      <span class="text-[13px] text-dim">opencode — <code class="font-mono text-[12px]">opencode.json</code></span>
      <div class="ml-auto flex items-center rounded-md border border-line bg-bg p-0.5" aria-label="OpenCode MCP catalog">
        <button
          class="rounded px-2.5 py-1 text-[11px] text-dim transition-colors hover:text-fg"
          class:!bg-surface-2={mcpCatalog === "core"}
          class:!text-fg={mcpCatalog === "core"}
          aria-pressed={mcpCatalog === "core"}
          onclick={() => (mcpCatalog = "core")}
        >Core</button>
        <button
          class="rounded px-2.5 py-1 text-[11px] text-dim transition-colors hover:text-fg"
          class:!bg-surface-2={mcpCatalog === "api"}
          class:!text-fg={mcpCatalog === "api"}
          aria-pressed={mcpCatalog === "api"}
          onclick={() => (mcpCatalog = "api")}
        >API</button>
        <button
          class="rounded px-2.5 py-1 text-[11px] text-dim transition-colors hover:text-fg"
          class:!bg-surface-2={mcpCatalog === "admin"}
          class:!text-fg={mcpCatalog === "admin"}
          aria-pressed={mcpCatalog === "admin"}
          onclick={() => (mcpCatalog = "admin")}
        >Admin</button>
      </div>
      <button class="btn btn-sm" onclick={() => copy(opencodeConfig, "oc")}>{copied === "oc" ? "Copied" : "Copy"}</button>
    </div>
    <pre class="m-0 overflow-x-auto rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{opencodeConfig}</pre>
    <p class="mb-0 mt-1.5 text-[11px] text-faint">
      {#if mcpCatalog === "admin"}
        Admin uses <code class="font-mono">{mcpPath}/admin</code> and exposes only mutation and configuration tools. Add Core or API separately when the same client also needs their read tools.
      {:else if mcpCatalog === "api"}
        API uses <code class="font-mono">{mcpPath}/api</code> and exposes only catalog discovery and <code class="font-mono">call_api_endpoint</code>.
      {:else}
        Core uses <code class="font-mono">{mcpPath}</code> and exposes only read-only codebase and documentation tools.
      {/if}
      Add this to project or user <code class="font-mono">opencode.json</code>, restart the client, then verify with <code class="font-mono">opencode mcp list</code>.
    </p>
  </div>

  <div class="mb-4">
    <div class="mb-1.5 flex items-center gap-2">
      <span class="text-[13px] text-dim">Claude Code — CLI</span>
      <button class="btn btn-sm ml-auto" onclick={() => copy(claudeCmd, "cc")}>{copied === "cc" ? "Copied" : "Copy"}</button>
    </div>
    <pre class="m-0 overflow-x-auto rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{claudeCmd}</pre>
    <p class="mb-0 mt-1.5 text-[11px] text-faint">Claude Code stores this in local project scope by default. Use <code class="font-mono">--scope user</code> for all projects, then verify with <code class="font-mono">claude mcp list</code>.</p>
  </div>

  <div>
    <div class="mb-1.5 flex items-center gap-2">
      <span class="text-[13px] text-dim">Cursor / VS Code / other — <code class="font-mono text-[12px]">mcpServers</code></span>
      <button class="btn btn-sm ml-auto" onclick={() => copy(genericConfig, "gen")}>{copied === "gen" ? "Copied" : "Copy"}</button>
    </div>
    <pre class="m-0 overflow-x-auto rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{genericConfig}</pre>
  </div>
</div>

<div class="card my-4 p-4">
  <h2 class="mb-1 text-[15px] font-semibold">How changes are picked up</h2>
  <p class="mt-0 text-[13px] text-faint">
    MCP tool changes and changes inside a catalogued API follow different update paths.
  </p>

  <div class="mt-3 grid gap-3 sm:grid-cols-2">
    <div class="rounded-md border border-line p-3">
      <h3 class="m-0 text-[13px] font-medium">Krabby / MCP updates</h3>
      <p class="mb-0 mt-1.5 text-[12px] text-dim">
        Each catalog keeps a stable endpoint URL across Krabby upgrades. MCP clients usually cache
        the tool list and its schemas for a session, so reconnect or restart the client after upgrading Krabby
        to discover added tools or changed arguments.
      </p>
    </div>

    <div class="rounded-md border border-line p-3">
      <h3 class="m-0 text-[13px] font-medium">Catalogued API updates</h3>
      <p class="mb-0 mt-1.5 text-[12px] text-dim">
        OpenAPI and gRPC definitions are refreshed manually with
        <code class="font-mono">refresh_api_service</code> or automatically on the service schedule. Krabby
        detects unchanged definitions, re-renders and reindexes changed or removed endpoints, then serves the
        current catalog through MCP immediately. Editing a base URL, spec patch, or operation override forces a
        full re-render even when the upstream definition did not change.
      </p>
    </div>
  </div>
</div>

<div class="card my-4 p-4">
  <h2 class="mb-1 text-[15px] font-semibold">Track a repository</h2>
  <p class="mt-0 text-[13px] text-faint">
    Enable both Core and Admin, then hand a git URL — HTTPS or SSH — to your agent. Admin performs the
    mutation; Core supplies repository discovery and build status.
  </p>

  <div class="mb-4 mt-3 rounded-md border border-accent/40 bg-accent/5 p-3">
    <div class="mb-1.5 flex items-center gap-2">
      <div>
        <div class="text-[13px] font-medium">Update-or-create prompt</div>
        <div class="text-[11px] text-faint">
          Replace the URL with your repo, then paste this into your agent.
        </div>
      </div>
      <button class="btn btn-sm ml-auto" onclick={() => copy(triggerPrompt, "trigger")}>
        {copied === "trigger" ? "Copied" : "Copy"}
      </button>
    </div>
    <pre class="m-0 whitespace-pre-wrap rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{triggerPrompt}</pre>
  </div>

  <p class="mt-0 text-[13px] text-dim">
    Under the hood the agent calls <code class="font-mono text-[12px]">list_repos</code> to check whether
    the repo exists, then <code class="font-mono text-[12px]">add_repo</code> to clone and build it, or
    <code class="font-mono text-[12px]">refresh_repo</code> to pull and rebuild an existing one. Poll
    <code class="font-mono text-[12px]">repo_status</code> until the build state is
    <code class="font-mono text-[12px]">ready</code>.
  </p>
  <p class="mb-0 mt-2 text-[13px] text-faint">
    Both HTTPS and SSH URLs work. Private repos need a git credential — store a token (HTTPS) or SSH key
    (SSH) with <code class="font-mono text-[12px]">set_credential</code>; the most specific host or
    host/path pattern wins.
  </p>

  <div class="mt-5 border-t border-line pt-4">
    <div class="mb-1 flex items-center gap-2">
      <h3 class="text-[13px] font-medium">Or call the REST API directly</h3>
      <code class="rounded border border-line px-1.5 py-0.5 font-mono text-[11px] text-fg">POST /api/v1/repos</code>
    </div>
    <p class="mt-0 text-[13px] text-faint">
      One endpoint does both: it creates the repo if it isn't tracked yet, or refreshes it (pull + rebuild)
      if it already exists. Send a JSON body with the git <code class="font-mono text-[12px]">url</code>
      (required) and an optional <code class="font-mono text-[12px]">branch</code> (empty = default branch).
      Returns <code class="font-mono text-[12px]">202 Accepted</code> with the repo record while the build
      runs in the background.
    </p>

    <div class="mb-1.5 mt-3 flex items-center gap-2">
      <span class="text-[13px] text-dim">Example — <code class="font-mono text-[12px]">curl</code></span>
      <button class="btn btn-sm ml-auto" onclick={() => copy(curlAddRepo, "curl")}>{copied === "curl" ? "Copied" : "Copy"}</button>
    </div>
    <pre class="m-0 overflow-x-auto rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{curlAddRepo}</pre>

    <p class="mb-0 mt-2 text-[13px] text-faint">
      This is the same endpoint the "Add repo" button uses, so it's safe to call repeatedly — an existing
      repo is simply queued for a refresh. Note the MCP <code class="font-mono text-[12px]">X-Api-Key</code>
      guards all three MCP endpoints under <code class="font-mono text-[12px]">{mcpPath}</code>, not the REST API.
    </p>
  </div>
</div>

<div class="card my-4 p-4">
  <div class="flex flex-wrap items-start gap-3">
    <div>
      <h2 class="mb-1 text-[15px] font-semibold">Tool catalogs</h2>
      <p class="m-0 text-[13px] text-faint">
        Each tool is published by exactly one endpoint. Select a catalog to inspect its tools.
      </p>
    </div>
    <div class="ml-auto flex items-center rounded-md border border-line bg-bg p-0.5" aria-label="Published MCP tool catalog">
      {#each [["core", 22], ["api", 5], ["admin", 38]] as [catalog, count] (catalog)}
        <button
          class="rounded px-2.5 py-1 text-[11px] capitalize text-dim transition-colors hover:text-fg"
          class:!bg-surface-2={mcpCatalog === catalog}
          class:!text-fg={mcpCatalog === catalog}
          aria-pressed={mcpCatalog === catalog}
          onclick={() => (mcpCatalog = catalog)}
        >{catalog} · {count}</button>
      {/each}
    </div>
  </div>

  <p class="mb-0 mt-3 rounded-md border border-line bg-bg px-3 py-2 text-[12px] text-dim">
    Published at <code class="font-mono text-fg">{mcpUrl}</code>
  </p>

  {#each visibleToolGroups as group (group.name)}
    <h3 class="mb-1.5 mt-4 text-[13px] font-medium uppercase tracking-wider text-faint">{group.name}</h3>
    <div class="overflow-hidden rounded-md border border-line">
      <table class="w-full border-collapse">
        <tbody>
          {#each group.tools as [name, desc] (name)}
            <tr class="border-b border-line last:border-b-0">
              <td class="w-[190px] px-3 py-2 align-top font-mono text-[12.5px] text-fg">{name}</td>
              <td class="px-3 py-2 text-[13px] text-dim">{desc}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/each}
</div>
