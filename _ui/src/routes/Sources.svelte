<script>
  // Web content sources: named collections (wikis, Confluence spaces) whose
  // pages are synced to markdown and indexed into the docs RAG. Each
  // collection is searchable on the Search page as "web:<name>".
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { path as routePath, navigate, link } from "../lib/router.js";
  import { fmtDate } from "../lib/format.js";
  import Icon from "../lib/Icon.svelte";
  import Status from "../lib/Status.svelte";
  import MarkdownView from "../lib/MarkdownView.svelte";
  import { successToast } from "../lib/toast.js";

  // selected/doc come from the route: /sources/<name>?doc=<path>.
  let { sourceName = "" } = $props();
  let docParam = $derived.by(() => {
    const params = new URLSearchParams($routePath.split("?")[1] || "");
    return params.get("doc") || "";
  });

  let sources = $state([]);
  let loaded = $state(false);
  let error = $state("");

  // Per-collection page lists, loaded lazily when expanded.
  let pages = $state({});
  let expanded = $state({});
  // Per-collection distinct team names and the active team filter (JIRA).
  let teams = $state({});
  let teamFilter = $state({});
  // Per-collection pagination: current page (1-based), total matching items and
  // whether more pages exist. The server pages the item list so large sources
  // (thousands of pages) are never loaded whole.
  const PER_PAGE = 50;
  let pageNum = $state({});
  let pageTotal = $state({});
  let pageHasMore = $state({});
  let pageLoading = $state({});

  // Doc viewer state.
  let docContent = $state("");
  let docError = $state("");

  // Add form.
  let showAdd = $state(false);
  let editingName = $state("");
  let adding = $state(false);
  let form = $state(newForm());

  function newForm() {
    return {
      name: "",
      type: "pages",
      description: "",
      refresh_interval: "24h",
      // Cron schedule(s), comma-separated (hardloop syntax, e.g. "0 2 * * *").
      // When set it is authoritative over refresh_interval, like repo schedules.
      schedule: "",
      base_url: "",
      space: "",
      user: "",
      api_token: "",
      include_labels: "",
      exclude_labels: "",
      // Confluence-only
      root_page: "",
      include_root: true,
      // JIRA-only
      project: "",
      jql: "",
      team_fields: "",
      max_issues: "",
      // Confluence + JIRA
      full_resync_every: "",
    };
  }

  async function load() {
    try {
      sources = (await api.sources()) || [];
      error = "";
    } catch (e) {
      error = e.message;
    } finally {
      loaded = true;
    }
  }

  // Poll the source list while any source is actively syncing/indexing, so the
  // progress bar and page counts update live without a manual refresh.
  let pollTimer = null;
  function anyRunning() {
    return sources.some((s) => s.running || s.status === "fetching" || s.progress);
  }
  $effect(() => {
    if (anyRunning() && !pollTimer) {
      pollTimer = setInterval(load, 2000);
    } else if (!anyRunning() && pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  });
  onMount(() => () => {
    if (pollTimer) clearInterval(pollTimer);
  });

  // Human progress label + percentage for a source's current phase.
  function progressPct(p) {
    if (!p || !p.total) return null;
    return Math.min(100, Math.round((p.done / p.total) * 100));
  }
  function progressLabel(p) {
    if (!p) return "";
    const phase = { fetch: "Fetching", write: "Saving", index: "Embedding" }[p.phase] || p.phase;
    if (!p.total) return `${phase}…`;
    return `${phase} ${p.done}/${p.total}`;
  }

  async function loadPages(name, page = pageNum[name] || 1) {
    pageLoading = { ...pageLoading, [name]: true };
    try {
      const res = await api.source(name, teamFilter[name] || "", page, PER_PAGE);
      pages = { ...pages, [name]: res?.pages || [] };
      pageNum = { ...pageNum, [name]: res?.page || page };
      pageTotal = { ...pageTotal, [name]: res?.total ?? (res?.pages?.length || 0) };
      pageHasMore = { ...pageHasMore, [name]: !!res?.has_more };
      // teams is the full distinct set across the collection (server-provided).
      if (res?.teams) teams = { ...teams, [name]: res.teams };
    } catch (e) {
      error = e.message;
    } finally {
      pageLoading = { ...pageLoading, [name]: false };
    }
  }

  function goToPage(name, page) {
    if (page < 1) return;
    pageNum = { ...pageNum, [name]: page };
    loadPages(name, page);
  }

  function setTeamFilter(name, value) {
    teamFilter = { ...teamFilter, [name]: value };
    pageNum = { ...pageNum, [name]: 1 }; // reset to first page on filter change
    loadPages(name, 1);
  }

  function toggle(name) {
    expanded = { ...expanded, [name]: !expanded[name] };
    if (expanded[name] && !pages[name]) loadPages(name, pageNum[name] || 1);
  }

  // Human 1-based range of the current page, e.g. "1–50 of 4634".
  function pageRange(name) {
    const total = pageTotal[name] || 0;
    if (total === 0) return "0";
    const p = pageNum[name] || 1;
    const from = (p - 1) * PER_PAGE + 1;
    const to = Math.min(p * PER_PAGE, total);
    return `${from}\u2013${to} of ${total}`;
  }

  function lastPage(name) {
    return Math.max(1, Math.ceil((pageTotal[name] || 0) / PER_PAGE));
  }

  function splitLabels(s) {
    return s
      .split(",")
      .map((x) => x.trim())
      .filter(Boolean);
  }

  async function add() {
    adding = true;
    try {
      let config = {};
      if (form.type === "confluence") {
        config = {
          base_url: form.base_url.trim(),
          space: form.space.trim(),
          root_page: form.root_page.trim(),
          include_root: form.include_root,
          user: form.user.trim(),
          api_token: form.api_token,
          include_labels: splitLabels(form.include_labels),
          exclude_labels: splitLabels(form.exclude_labels),
          full_resync_every: form.full_resync_every.trim(),
        };
      } else if (form.type === "jira") {
        config = {
          base_url: form.base_url.trim(),
          user: form.user.trim(),
          api_token: form.api_token,
          project: form.project.trim(),
          jql: form.jql.trim(),
          include_labels: splitLabels(form.include_labels),
          exclude_labels: splitLabels(form.exclude_labels),
          team_fields: splitLabels(form.team_fields),
          max_issues: form.max_issues ? Number(form.max_issues) : 0,
          full_resync_every: form.full_resync_every.trim(),
        };
      }
      const specs = form.schedule
        .split(",")
        .map((x) => x.trim())
        .filter(Boolean);
      const body = {
        name: form.name.trim(),
        type: form.type,
        description: form.description.trim(),
        // A cron schedule, when given, is authoritative; otherwise the interval.
        refresh_interval:
          specs.length || form.refresh_interval === "manual" ? "" : form.refresh_interval,
        specs,
        config,
      };
      if (editingName) await api.updateSource(editingName, body);
      else await api.addSource(body);
      form = newForm();
      editingName = "";
      showAdd = false;
      error = "";
      await load();
    } catch (e) {
      error = e.message;
    } finally {
      adding = false;
    }
  }

  function editSource(source, e) {
    e.stopPropagation();
    editingName = source.name;
    form = {
      name: source.name,
      type: source.type,
      description: source.description || "",
      refresh_interval: source.refresh_interval || "manual",
      schedule: (source.specs || []).join(", "),
      base_url: source.config?.base_url || "",
      space: source.config?.space || "",
      user: source.config?.user || "",
      api_token: "",
      include_labels: (source.config?.include_labels || []).join(", "),
      exclude_labels: (source.config?.exclude_labels || []).join(", "),
      root_page: source.config?.root_page || "",
      include_root: source.config?.include_root !== false,
      project: source.config?.project || "",
      jql: source.config?.jql || "",
      team_fields: (source.config?.team_fields || []).join(", "),
      max_issues: source.config?.max_issues || "",
      full_resync_every: source.config?.full_resync_every || "",
    };
    showAdd = true;
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  function closeForm() {
    showAdd = false;
    editingName = "";
    form = newForm();
  }

  async function refresh(name, e) {
    e.stopPropagation();
    try {
      await api.refreshSource(name);
      successToast("Sync queued");
      await load();
    } catch (err) {
      error = err.message;
    }
  }

  async function remove(name, e) {
    e.stopPropagation();
    if (!confirm(`Delete source "${name}", its synced pages and index entries?`)) return;
    try {
      await api.deleteSource(name);
      await load();
    } catch (err) {
      error = err.message;
    }
  }

  // Per-collection "add page" inputs (pages type).
  let pageUrl = $state({});

  async function addPage(name) {
    const url = (pageUrl[name] || "").trim();
    if (!url) return;
    try {
      await api.addSourcePage(name, url);
      pageUrl = { ...pageUrl, [name]: "" };
      await loadPages(name);
      await load();
    } catch (e) {
      error = e.message;
    }
  }

  async function removePage(name, slug) {
    if (!confirm(`Remove page ${slug}?`)) return;
    try {
      await api.deleteSourcePage(name, slug);
      await loadPages(name);
      await load();
    } catch (e) {
      error = e.message;
    }
  }

  // Load the markdown of the routed doc (deep link from search results).
  $effect(() => {
    const name = sourceName;
    const doc = docParam;
    if (!name || !doc) {
      docContent = "";
      docError = "";
      return;
    }
    docContent = "";
    docError = "";
    api
      .sourceDoc(name, doc)
      .then((res) => (docContent = res?.content || ""))
      .catch((e) => (docError = e.message));
  });

  function sourceOfDoc() {
    return sources.find((s) => s.name === sourceName);
  }

  function docPageURL() {
    const list = pages[sourceName] || [];
    const slug = docParam.replace(/\.md$/, "");
    return list.find((p) => p.slug === slug)?.url || "";
  }

  $effect(() => {
    if (sourceName && docParam && !pages[sourceName]) loadPages(sourceName);
  });

  onMount(load);
</script>

{#if sourceName && docParam}
  <!-- Doc viewer: /sources/<name>?doc=<path> -->
  <div class="mb-3 flex items-center gap-2 text-[13px]">
    <a href="/sources" use:link class="text-dim transition-colors hover:text-fg">Sources</a>
    <span class="text-faint">/</span>
    <span class="font-mono">{sourceName}</span>
    <span class="text-faint">/</span>
    <span class="truncate font-mono text-dim">{docParam}</span>
    {#if docPageURL()}
      <a class="btn btn-sm ml-auto" href={docPageURL()} target="_blank" rel="noreferrer noopener">
        Open original
      </a>
    {/if}
  </div>
  {#if docError}
    <div class="card p-6 text-center text-err">{docError}</div>
  {:else if !docContent}
    <div class="card p-6 text-center text-dim">Loading…</div>
  {:else}
    <div class="card p-5">
      <MarkdownView markdown={docContent} />
    </div>
  {/if}
{:else}
  <p class="text-dim">
    Non-git content sources: wikis and Confluence spaces synced to markdown and searchable via docs
    search as <code class="font-mono text-[12px]">web:&lt;name&gt;</code>.
  </p>

  {#if error}
    <div class="mt-3 rounded-md border border-err bg-err/10 px-3 py-2.5 text-[13px] text-err">{error}</div>
  {/if}

  <div class="my-4">
    {#if !showAdd}
      <button class="btn btn-primary" onclick={() => (showAdd = true)}>Add source</button>
    {:else}
      <div class="card flex flex-col gap-3 p-4">
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Name (search scope)
            <input class="input" placeholder="e.g. wine" bind:value={form.name} disabled={!!editingName} />
          </label>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Type
            <select class="input" bind:value={form.type} disabled={!!editingName}>
              <option value="pages">Custom web (URL list)</option>
              <option value="confluence">Confluence space</option>
              <option value="jira">JIRA project / JQL</option>
            </select>
          </label>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Auto refresh {form.schedule.trim() ? "(overridden by schedule)" : ""}
            <select class="input" bind:value={form.refresh_interval} disabled={!!form.schedule.trim()}>
              <option value="manual">manual only</option>
              <option value="1h">every hour</option>
              <option value="6h">every 6 hours</option>
              <option value="24h">daily</option>
              <option value="168h">weekly</option>
            </select>
          </label>
        </div>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Cron schedule (optional; comma-separated, overrides auto refresh — same as repos)
          <input
            class="input font-mono"
            placeholder="0 2 * * *,  @every 6h"
            bind:value={form.schedule}
          />
          <span class="text-[11px] text-faint">
            e.g. <code class="font-mono">0 2 * * *</code> (daily 02:00),
            <code class="font-mono">@every 6h</code>, or several separated by commas. Leave empty to use Auto refresh.
          </span>
        </label>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Description (what this source holds — shown to MCP/AI to pick the right source)
          <input
            class="input"
            placeholder="e.g. Delivery Support runbooks and TERs"
            bind:value={form.description}
          />
        </label>

        {#if form.type === "confluence"}
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Base URL
              <input class="input" placeholder="https://acme.atlassian.net/wiki" bind:value={form.base_url} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Space key {form.root_page ? "(optional when root page set)" : ""}
              <input class="input" placeholder="FinOps" bind:value={form.space} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Root page id (index this page + all descendants only)
              <input class="input" placeholder="1254228318" bind:value={form.root_page} />
            </label>
            {#if form.root_page}
              <label class="flex items-center gap-2 text-[13px] text-dim sm:col-span-2">
                <input type="checkbox" bind:checked={form.include_root} />
                Also index the root page itself
              </label>
            {/if}
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              User (email; empty = bearer token)
              <input class="input" placeholder="me@acme.com" bind:value={form.user} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              API token {editingName ? "(blank = keep existing)" : ""}
              <input class="input" type="password" bind:value={form.api_token} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Include labels (comma separated; empty = all pages)
              <input class="input" placeholder="public, docs" bind:value={form.include_labels} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Exclude labels (comma separated)
              <input class="input" placeholder="draft, archived" bind:value={form.exclude_labels} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Full re-sync every (e.g. 24h; reconciles deletions)
              <input class="input" placeholder="24h" bind:value={form.full_resync_every} />
            </label>
          </div>
          <p class="m-0 text-[12px] text-faint">
            Set a <strong>Root page id</strong> (from the page URL) to index just that page and its
            whole sub-tree — register several sub-trees of one space as separate keyed sources (e.g.
            <code class="font-mono">delivery-support</code>). Leave it empty to index the whole space.
          </p>
        {:else if form.type === "jira"}
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Base URL
              <input class="input" placeholder="https://jira.acme.com" bind:value={form.base_url} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Project key (or use JQL below)
              <input class="input" placeholder="OFS" bind:value={form.project} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim sm:col-span-2">
              JQL (optional; overrides project)
              <input
                class="input"
                placeholder="project = OFS AND updated >= -30d ORDER BY updated DESC"
                bind:value={form.jql}
              />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              User (email; empty = bearer token / PAT)
              <input class="input" placeholder="me@acme.com" bind:value={form.user} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              API token / PAT {editingName ? "(blank = keep existing)" : ""}
              <input class="input" type="password" bind:value={form.api_token} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Include labels (comma separated; empty = all)
              <input class="input" placeholder="customer, prod" bind:value={form.include_labels} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Skip labels (comma separated)
              <input class="input" placeholder="wontfix, duplicate" bind:value={form.exclude_labels} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Team field ids (comma separated custom fields)
              <input class="input" placeholder="customfield_104705, customfield_110643" bind:value={form.team_fields} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Max issues (0 = default)
              <input class="input" type="number" min="0" placeholder="5000" bind:value={form.max_issues} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Full re-sync every (e.g. 24h; reconciles deletions)
              <input class="input" placeholder="24h" bind:value={form.full_resync_every} />
            </label>
          </div>
          <p class="m-0 text-[12px] text-faint">
            Team field ids are instance-specific JIRA custom fields that hold team/squad ownership
            (e.g. a "Squad" field). Their values are indexed so tickets are searchable by team name.
          </p>
        {:else}
          <p class="m-0 text-[12px] text-faint">
            Add page URLs after creating the collection. Private pages resolve auth from the git
            credentials store by URL pattern.
          </p>
        {/if}

        <div class="flex gap-2">
          <button class="btn btn-primary" onclick={add} disabled={adding || !form.name.trim()}>
            {adding ? "Saving…" : editingName ? "Save source" : "Create source"}
          </button>
          <button class="btn" onclick={closeForm}>Cancel</button>
        </div>
      </div>
    {/if}
  </div>

  <div class="flex flex-col gap-3">
    {#if !loaded}
      <div class="card p-6 text-center text-dim">Loading…</div>
    {:else if sources.length === 0}
      <div class="card p-6 text-center text-dim">No web sources yet.</div>
    {:else}
      {#each sources as s (s.name)}
        <div class="card overflow-hidden">
          <button
            class="flex w-full cursor-pointer items-center gap-2.5 px-3.5 py-2.5 text-left hover:bg-surface-2"
            onclick={() => toggle(s.name)}
            aria-expanded={!!expanded[s.name]}
          >
            <Icon name={expanded[s.name] ? "chevron-down" : "chevron-right"} size={14} />
            <Icon name={s.type === "confluence" ? "book" : s.type === "jira" ? "tag" : "search"} size={14} />
            <span class="font-mono text-[13.5px] font-medium">{s.name}</span>
            <span class="rounded border border-line px-1.5 text-[11px] text-dim">{s.type}</span>
            <span class="font-mono text-[11px] text-faint">web:{s.name}</span>
            <span class="ml-auto flex items-center gap-2.5 text-[12px] text-faint">
              {#if s.progress}
                <span class="flex items-center gap-1.5 text-busy">
                  {#if progressPct(s.progress) !== null}
                    <span class="inline-block h-1.5 w-20 overflow-hidden rounded-full bg-surface-3">
                      <span
                        class="block h-full rounded-full bg-busy transition-all"
                        style="width: {progressPct(s.progress)}%"
                      ></span>
                    </span>
                    <span class="font-mono">{progressLabel(s.progress)} ({progressPct(s.progress)}%)</span>
                  {:else}
                    <span>{progressLabel(s.progress)}</span>
                  {/if}
                </span>
              {:else if s.running}
                <span class="text-busy">({s.running})</span>
              {/if}
              <span>{s.page_count} {s.page_count === 1 ? "page" : "pages"}</span>
              <Status status={s.status} />
            </span>
          </button>

          {#if expanded[s.name]}
            <div class="border-t border-line px-3.5 py-3">
              {#if s.description}
                <p class="mb-2 text-[12.5px] text-dim">{s.description}</p>
              {/if}
              <div class="mb-3 flex flex-wrap items-center gap-x-5 gap-y-1 text-[12px] text-faint">
                <span>Last sync: {s.last_refresh_at ? fmtDate(s.last_refresh_at) : "never"}</span>
                {#if s.specs?.length}
                  <span>Schedule: <span class="font-mono">{s.specs.join(", ")}</span></span>
                {:else}
                  <span>Auto refresh: {s.refresh_interval || "manual"}</span>
                {/if}
                {#if s.type === "confluence"}
                  <span class="font-mono">
                    {s.config?.base_url}
                    {s.config?.root_page ? `· page ${s.config.root_page} + subtree` : `· ${s.config?.space}`}
                  </span>
                  {#if s.config?.include_labels?.length}
                    <span>labels: {s.config.include_labels.join(", ")}</span>
                  {/if}
                {/if}
                {#if s.type === "jira"}
                  <span class="font-mono">{s.config?.base_url} · {s.config?.jql || s.config?.project}</span>
                  {#if s.config?.exclude_labels?.length}
                    <span>skip: {s.config.exclude_labels.join(", ")}</span>
                  {/if}
                {/if}
                {#if s.last_error}
                  <span class="text-err" title={s.last_error}>error: {s.last_error.slice(0, 120)}</span>
                {/if}
                <span class="ml-auto flex gap-1.5">
                  <button class="btn btn-sm" onclick={(e) => refresh(s.name, e)}>Sync now</button>
                  <button class="btn btn-sm" onclick={(e) => editSource(s, e)}>Edit</button>
                  <button class="btn btn-sm btn-danger" onclick={(e) => remove(s.name, e)}>Delete</button>
                </span>
              </div>

              {#if s.type === "pages"}
                <div class="mb-3 flex gap-2">
                  <input
                    class="input flex-1"
                    placeholder="https://wiki.example.com/page"
                    value={pageUrl[s.name] || ""}
                    oninput={(e) => (pageUrl = { ...pageUrl, [s.name]: e.target.value })}
                    onkeydown={(e) => e.key === "Enter" && addPage(s.name)}
                  />
                  <button class="btn" onclick={() => addPage(s.name)} disabled={!(pageUrl[s.name] || "").trim()}>
                    Add page
                  </button>
                </div>
              {/if}

              {#if s.type === "jira" && teams[s.name]?.length}
                <div class="mb-3 flex items-center gap-2 text-[12px] text-dim">
                  <span>Filter by team:</span>
                  <select
                    class="input max-w-xs"
                    value={teamFilter[s.name] || ""}
                    onchange={(e) => setTeamFilter(s.name, e.target.value)}
                  >
                    <option value="">all teams</option>
                    {#each teams[s.name] as t}
                      <option value={t}>{t}</option>
                    {/each}
                  </select>
                </div>
              {/if}

              {#if !(pages[s.name]?.length)}
                <div class="py-3 text-center text-[13px] text-dim">
                  {pages[s.name] ? "No pages synced yet." : "Loading…"}
                </div>
              {:else}
                <table class="w-full border-collapse">
                  <tbody>
                    {#each pages[s.name] as p (p.id)}
                      <tr class="hover:bg-surface-2">
                        <td class="border-b border-line px-2 py-1.5">
                          <button
                            class="cursor-pointer text-left font-mono text-[12.5px] hover:text-accent"
                            onclick={() => navigate(`/sources/${s.name}?doc=${encodeURIComponent(p.slug + ".md")}`)}
                            title={p.url}
                          >
                            {p.title || p.slug}
                          </button>
                        </td>
                        <td class="border-b border-line px-2 py-1.5"><Status status={p.status} dot /></td>
                        <td class="border-b border-line px-2 py-1.5 text-[11px] text-faint">{fmtDate(p.last_fetch_at)}</td>
                        <td class="border-b border-line px-2 py-1.5 text-right">
                          {#if p.last_error}
                            <span class="mr-2 text-[11px] text-err" title={p.last_error}>fetch failed</span>
                          {/if}
                          <a class="mr-1 text-[11px] text-dim hover:text-fg" href={p.url} target="_blank" rel="noreferrer noopener">open</a>
                          {#if s.type === "pages"}
                            <button class="btn btn-sm btn-danger" onclick={() => removePage(s.name, p.slug)}>Remove</button>
                          {/if}
                        </td>
                      </tr>
                    {/each}
                  </tbody>
                </table>

                <!-- Pagination: items are paged server-side so large sources
                     (thousands of pages) load one window at a time. -->
                {#if (pageTotal[s.name] || 0) > PER_PAGE}
                  <div class="mt-2.5 flex items-center justify-between text-[12px] text-dim">
                    <span>{pageRange(s.name)}</span>
                    <div class="flex items-center gap-1">
                      <button
                        class="btn btn-sm"
                        disabled={(pageNum[s.name] || 1) <= 1 || pageLoading[s.name]}
                        onclick={() => goToPage(s.name, 1)}
                        title="First page"
                      >
                        «
                      </button>
                      <button
                        class="btn btn-sm"
                        disabled={(pageNum[s.name] || 1) <= 1 || pageLoading[s.name]}
                        onclick={() => goToPage(s.name, (pageNum[s.name] || 1) - 1)}
                      >
                        Prev
                      </button>
                      <span class="px-1.5 font-mono">
                        {pageNum[s.name] || 1} / {lastPage(s.name)}
                      </span>
                      <button
                        class="btn btn-sm"
                        disabled={!pageHasMore[s.name] || pageLoading[s.name]}
                        onclick={() => goToPage(s.name, (pageNum[s.name] || 1) + 1)}
                      >
                        Next
                      </button>
                      <button
                        class="btn btn-sm"
                        disabled={!pageHasMore[s.name] || pageLoading[s.name]}
                        onclick={() => goToPage(s.name, lastPage(s.name))}
                        title="Last page"
                      >
                        »
                      </button>
                    </div>
                  </div>
                {/if}
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    {/if}
  </div>
{/if}
