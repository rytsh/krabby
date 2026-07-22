<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";

  let settings = $state(null);

  let mcpPath = $derived((settings && settings.mcp && settings.mcp.path) || "/mcp");
  let apiKeySet = $derived(!!(settings && settings.mcp && settings.mcp.api_key_set));
  let mcpUrl = $derived(`${window.location.origin}${mcpPath}`);

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
  "mcp": {
    "krabby": {
      "type": "remote",
      "url": "${mcpUrl}"${apiKeySet ? `,\n      "headers": { "X-Api-Key": "<your-api-key>" }` : ""}
    }
  }
}`);

  let claudeCmd = $derived(`claude mcp add --transport http krabby ${mcpUrl}${apiKeySet ? ' --header "X-Api-Key: <your-api-key>"' : ""}`);

  let genericConfig = $derived(`{
  "mcpServers": {
    "krabby": {
      "type": "http",
      "url": "${mcpUrl}"${apiKeySet ? `,\n      "headers": { "X-Api-Key": "<your-api-key>" }` : ""}
    }
  }
}`);

  let installPrompt = $derived(`Connect this AI client to the Krabby remote MCP server.

Server name: krabby
Transport: streamable HTTP
URL: ${mcpUrl}
${apiKeySet ? "Authentication: send the API key in the X-Api-Key header. Ask me for the key before editing the configuration." : "Authentication: none"}

Detect this client's MCP configuration format and update the appropriate project or user configuration. Preserve all existing settings and other MCP servers. After configuring it, verify the connection and confirm that the Krabby tools are available.`);

  const triggerRepoUrl = "https://github.com/owner/repo";
  let triggerPrompt = $derived(`Using the Krabby MCP tools, track this repository: ${triggerRepoUrl}

If it is already tracked, refresh it to pull the latest commits and rebuild its knowledge graph. Otherwise add it. Then report the final build status.

The URL can be HTTPS or SSH (e.g. git@github.com:owner/repo.git). For private repos make sure a matching git credential is stored first — a token for HTTPS or an SSH key for SSH URLs.`);

  const toolGroups = [
    {
      name: "Repositories",
      tools: [
        ["list_repos", "List tracked repositories with build status, last commit and last build time."],
        ["add_repo", "Track a new repository: clones it and builds its knowledge graph."],
        ["remove_repo", "Stop tracking a repository and delete its local clone and graph."],
        ["refresh_repo", "Pull the latest commits and rebuild the knowledge graph."],
        ["repo_status", "Get build state, last commit and last error of a repository."],
        ["lock_repo", "Take a TTL-bounded read lock so external tools can walk the clone safely."],
        ["unlock_repo", "Release a read lock taken with lock_repo."],
      ],
    },
    {
      name: "Knowledge graph",
      tools: [
        ["query_graph", "Search the code knowledge graph with BFS/DFS. Best first call for any codebase question."],
        ["get_node", "Get full details for a node by label or ID."],
        ["get_neighbors", "Get all direct neighbors of a node with edge details."],
        ["get_community", "Get all nodes in a community by community ID."],
        ["god_nodes", "The most connected nodes — the core abstractions of the codebase."],
        ["graph_stats", "Node count, edge count, communities, confidence breakdown."],
        ["shortest_path", "Find the shortest path between two concepts in the graph."],
      ],
    },
    {
      name: "Files",
      tools: [
        ["list_files", "List files and directories inside a tracked repository's clone."],
        ["read_file", "Read the source of a file inside a tracked repository's clone."],
      ],
    },
    {
      name: "Docs & search",
      tools: [
        ["list_docs", "List the generated markdown documentation files for a repository."],
        ["get_doc", "Return one whole generated markdown document by its doc path."],
        ["search_docs", "RAG search over generated markdown documentation."],
        ["search_code", "Normal bw full-text or semantic source search, with locations and pagination."],
      ],
    },
    {
      name: "Configuration",
      tools: [
        ["get_docs_config", "Return the current docs/RAG configuration (secrets redacted)."],
        ["set_docs_config", "Update the docs/RAG configuration and rebuild the clients live."],
        ["test_llm", "Test the chat LLM connection without saving."],
        ["test_embedder", "Test the embeddings connection without saving."],
        ["test_code_embedder", "Test the dedicated code embeddings connection without saving."],
        ["set_credential", "Store a git credential (SSH key or token) for a host or host/path prefix."],
        ["list_credentials", "List stored git credential patterns (secrets never returned)."],
        ["remove_credential", "Remove a stored git credential by its pattern."],
      ],
    },
  ];

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
  indexes on top. Everything you see in this UI is also available to AI agents through the
  <span class="text-fg">Model Context Protocol</span> — point your agent at the endpoint below and it can
  query graphs, read files and search docs across every tracked repo.
</p>

<div class="card my-4 p-4">
  <div class="mb-2 flex items-center gap-2">
    <h2 class="text-[15px] font-semibold">MCP endpoint</h2>
    <span class="rounded border border-line px-1.5 py-0.5 text-[11px] text-faint">streamable HTTP</span>
  </div>
  <div class="flex items-center gap-2">
    <code class="rounded-md border border-line bg-bg px-3 py-2 font-mono text-[13px]">{mcpUrl}</code>
    <button class="btn btn-sm" onclick={() => copy(mcpUrl, "url")}>{copied === "url" ? "Copied" : "Copy"}</button>
  </div>
  {#if apiKeySet}
    <p class="mb-0 mt-2 text-[13px] text-warn">
      An API key is configured: clients must send it in the <code class="font-mono">X-Api-Key</code> header.
    </p>
  {:else}
    <p class="mb-0 mt-2 text-[13px] text-faint">
      No API key configured — the endpoint is open. Set <code class="font-mono">KRABBY_MCP_API_KEY</code> to protect it.
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
    <div class="mb-1.5 flex items-center gap-2">
      <span class="text-[13px] text-dim">opencode — <code class="font-mono text-[12px]">opencode.json</code></span>
      <button class="btn btn-sm ml-auto" onclick={() => copy(opencodeConfig, "oc")}>{copied === "oc" ? "Copied" : "Copy"}</button>
    </div>
    <pre class="m-0 overflow-x-auto rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{opencodeConfig}</pre>
  </div>

  <div class="mb-4">
    <div class="mb-1.5 flex items-center gap-2">
      <span class="text-[13px] text-dim">Claude Code — CLI</span>
      <button class="btn btn-sm ml-auto" onclick={() => copy(claudeCmd, "cc")}>{copied === "cc" ? "Copied" : "Copy"}</button>
    </div>
    <pre class="m-0 overflow-x-auto rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{claudeCmd}</pre>
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
  <h2 class="mb-1 text-[15px] font-semibold">Track a repository</h2>
  <p class="mt-0 text-[13px] text-faint">
    Hand a git URL — HTTPS or SSH — to your agent and let it add the repo, or refresh it if it already exists.
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
</div>

<div class="card my-4 p-4">
  <h2 class="mb-1 text-[15px] font-semibold">Available tools</h2>
  <p class="mt-0 text-[13px] text-faint">
    {toolGroups.reduce((n, g) => n + g.tools.length, 0)} tools exposed over MCP.
  </p>

  {#each toolGroups as group}
    <h3 class="mb-1.5 mt-4 text-[13px] font-medium uppercase tracking-wider text-faint">{group.name}</h3>
    <div class="overflow-hidden rounded-md border border-line">
      <table class="w-full border-collapse">
        <tbody>
          {#each group.tools as [name, desc]}
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
