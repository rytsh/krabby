<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";

  let settings = $state(null);

  let basePath = $derived((settings && settings.server && settings.server.base_path) || "");
  let mcpPath = $derived((settings && settings.mcp && settings.mcp.path) || "/mcp");
  let apiKeySet = $derived(!!(settings && settings.mcp && settings.mcp.api_key_set));
  let mcpUrl = $derived(`${window.location.origin}${basePath}${mcpPath}`);
  let apiBase = $derived(`${window.location.origin}${basePath}/api/v1`);
  let mcpProfile = $state("standard");
  let configHeaders = $derived.by(() => {
    const headers = [];
    if (apiKeySet) headers.push(`"X-Api-Key": "<your-api-key>"`);
    if (mcpProfile === "full") headers.push(`"X-Krabby-Tool-Profile": "full"`);
    return headers.length ? `,\n      "headers": { ${headers.join(", ")} }` : "";
  });
  let cliHeaders = $derived(
    `${apiKeySet ? ' --header "X-Api-Key: <your-api-key>"' : ""}${mcpProfile === "full" ? ' --header "X-Krabby-Tool-Profile: full"' : ""}`,
  );

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
    "krabby": {
      "type": "remote",
      "url": "${mcpUrl}"${configHeaders}
    }
  }
}`);

  let claudeCmd = $derived(`claude mcp add --transport http krabby ${mcpUrl}${cliHeaders}`);

  let genericConfig = $derived(`{
  "mcpServers": {
    "krabby": {
      "type": "http",
      "url": "${mcpUrl}"${configHeaders}
    }
  }
}`);

  let installPrompt = $derived(`Connect this AI client to the Krabby remote MCP server.

Server name: krabby
Transport: streamable HTTP
URL: ${mcpUrl}
Tool profile: ${mcpProfile}${mcpProfile === "full" ? " (send X-Krabby-Tool-Profile: full on every request)" : " (default; no profile header)"}
${apiKeySet ? "Authentication: send the API key in the X-Api-Key header. Ask me for the key before editing the configuration." : "Authentication: none"}

Detect this client's MCP configuration format and update the appropriate project or user configuration. Preserve all existing settings and other MCP servers. After configuring it, verify the connection and confirm that the Krabby tools are available.`);

  const triggerRepoUrl = "https://github.com/owner/repo";
  let triggerPrompt = $derived(`Using the Krabby MCP tools, track this repository: ${triggerRepoUrl}

If it is already tracked, refresh it to pull the latest commits and rebuild its knowledge graph. Otherwise add it. Then report the final build status.

The URL can be HTTPS or SSH (e.g. git@github.com:owner/repo.git). For private repos make sure a matching git credential is stored first — a token for HTTPS or an SSH key for SSH URLs.`);

  let curlAddRepo = $derived(`curl -X POST ${apiBase}/repos \\
  -H "Content-Type: application/json" \\
  -d '{"url": "https://github.com/owner/repo", "branch": ""}'`);

  const toolGroups = [
    {
      name: "Repositories",
      tools: [
        ["list_repos", "List tracked repositories with build status, last commit and last build time."],
        ["add_repo", "Track a new repository: clones it and builds its knowledge graph."],
        ["remove_repo", "Stop tracking a repository and delete its local clone and graph."],
        ["refresh_repo", "Pull the latest commits and rebuild the knowledge graph."],
        ["repo_status", "Get build state, last commit and last error of a repository."],
        ["cancel_repo_job", "Cancel the refresh or generation job currently running for a repository."],
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
      name: "Files",
      tools: [
        ["list_files", "Inspect a bounded page of files and directories in a tracked clone."],
        ["read_file", "Read a bounded page of a known source file."],
      ],
    },
    {
      name: "Docs & search",
      tools: [
        ["list_docs", "Page through generated documentation metadata for a repository."],
        ["get_doc", "Read a bounded page of a known generated document."],
        ["search_docs", "Return ranked excerpts from repo docs and named web-source collections."],
        ["list_sources", "List Custom web and Confluence collections and their web:<name> search keys."],
        ["search_code", "First choice for symbols, paths, definitions, usages, and implementation locations."],
      ],
    },
    {
      name: "Configuration",
      tools: [
        ["get_docs_config", "Return the current docs/RAG configuration (secrets redacted).", true],
        ["set_docs_config", "Update the docs/RAG configuration and rebuild the clients live.", true],
        ["test_llm", "Test the chat LLM connection without saving.", true],
        ["test_embedder", "Test the embeddings connection without saving.", true],
        ["test_code_embedder", "Test the dedicated code embeddings connection without saving.", true],
        ["set_credential", "Store a git credential (SSH key or token) for a host or host/path prefix.", true],
        ["list_credentials", "List stored git credential patterns (secrets never returned).", true],
        ["remove_credential", "Remove a stored git credential by its pattern.", true],
      ],
    },
  ];
  let visibleToolGroups = $derived(
    toolGroups
      .map((group) => ({ ...group, tools: group.tools.filter(([, , fullOnly]) => !fullOnly || mcpProfile === "full") }))
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
  <div class="mt-3 grid gap-2 sm:grid-cols-2">
    <div class="rounded-md border border-accent/40 bg-accent/5 p-3">
      <div class="text-[13px] font-medium">Standard profile</div>
      <div class="mt-1 text-[12px] text-dim">Default when no profile header is sent. Exposes 28 repo, query, search, and read tools.</div>
    </div>
    <div class="rounded-md border border-line p-3">
      <div class="text-[13px] font-medium">Full profile</div>
      <div class="mt-1 text-[12px] text-dim">Uses the same URL with <code class="font-mono">X-Krabby-Tool-Profile: full</code>. Adds credential and docs/RAG administration tools.</div>
    </div>
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
    <div class="mb-1.5 flex flex-wrap items-center gap-2">
      <span class="text-[13px] text-dim">opencode — <code class="font-mono text-[12px]">opencode.json</code></span>
      <div class="ml-auto flex items-center rounded-md border border-line bg-bg p-0.5" aria-label="OpenCode MCP tool profile">
        <button
          class="rounded px-2.5 py-1 text-[11px] text-dim transition-colors hover:text-fg"
          class:!bg-surface-2={mcpProfile === "standard"}
          class:!text-fg={mcpProfile === "standard"}
          aria-pressed={mcpProfile === "standard"}
          onclick={() => (mcpProfile = "standard")}
        >Standard</button>
        <button
          class="rounded px-2.5 py-1 text-[11px] text-dim transition-colors hover:text-fg"
          class:!bg-surface-2={mcpProfile === "full"}
          class:!text-fg={mcpProfile === "full"}
          aria-pressed={mcpProfile === "full"}
          onclick={() => (mcpProfile = "full")}
        >Full</button>
      </div>
      <button class="btn btn-sm" onclick={() => copy(opencodeConfig, "oc")}>{copied === "oc" ? "Copied" : "Copy"}</button>
    </div>
    <pre class="m-0 overflow-x-auto rounded-md border border-line bg-bg p-3 font-mono text-[12.5px] leading-relaxed">{opencodeConfig}</pre>
    <p class="mb-0 mt-1.5 text-[11px] text-faint">
      {#if mcpProfile === "full"}
        Full adds <code class="font-mono">X-Krabby-Tool-Profile: full</code> under <code class="font-mono">headers</code> and exposes all 42 tools.
      {:else}
        Standard omits the profile header and exposes the smaller 28-tool catalog.
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
      only guards the <code class="font-mono text-[12px]">{mcpPath}</code> endpoint, not the REST API.
    </p>
  </div>
</div>

<div class="card my-4 p-4">
  <h2 class="mb-1 text-[15px] font-semibold">Available tools</h2>
  <p class="mt-0 text-[13px] text-faint">
    {visibleToolGroups.reduce((n, g) => n + g.tools.length, 0)} tools in the selected {mcpProfile} profile.
  </p>

  {#each visibleToolGroups as group}
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
