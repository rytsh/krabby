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
      refresh_interval: "24h",
      base_url: "",
      space: "",
      user: "",
      api_token: "",
      include_labels: "",
      exclude_labels: "",
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

  async function loadPages(name) {
    try {
      const res = await api.source(name);
      pages = { ...pages, [name]: res?.pages || [] };
    } catch (e) {
      error = e.message;
    }
  }

  function toggle(name) {
    expanded = { ...expanded, [name]: !expanded[name] };
    if (expanded[name]) loadPages(name);
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
      const body = {
        name: form.name.trim(),
        type: form.type,
        refresh_interval: form.refresh_interval === "manual" ? "" : form.refresh_interval,
        config: form.type === "confluence" ? {
          base_url: form.base_url.trim(),
          space: form.space.trim(),
          user: form.user.trim(),
          api_token: form.api_token,
          include_labels: splitLabels(form.include_labels),
          exclude_labels: splitLabels(form.exclude_labels),
        } : {},
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
      refresh_interval: source.refresh_interval || "manual",
      base_url: source.config?.base_url || "",
      space: source.config?.space || "",
      user: source.config?.user || "",
      api_token: "",
      include_labels: (source.config?.include_labels || []).join(", "),
      exclude_labels: (source.config?.exclude_labels || []).join(", "),
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
            </select>
          </label>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Auto refresh
            <select class="input" bind:value={form.refresh_interval}>
              <option value="manual">manual only</option>
              <option value="1h">every hour</option>
              <option value="6h">every 6 hours</option>
              <option value="24h">daily</option>
              <option value="168h">weekly</option>
            </select>
          </label>
        </div>

        {#if form.type === "confluence"}
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Base URL
              <input class="input" placeholder="https://acme.atlassian.net/wiki" bind:value={form.base_url} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Space key
              <input class="input" placeholder="WINE" bind:value={form.space} />
            </label>
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
          </div>
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
            <Icon name={s.type === "confluence" ? "book" : "search"} size={14} />
            <span class="font-mono text-[13.5px] font-medium">{s.name}</span>
            <span class="rounded border border-line px-1.5 text-[11px] text-dim">{s.type}</span>
            <span class="font-mono text-[11px] text-faint">web:{s.name}</span>
            <span class="ml-auto flex items-center gap-2.5 text-[12px] text-faint">
              {#if s.running}
                <span class="text-busy">({s.running})</span>
              {/if}
              <span>{s.page_count} {s.page_count === 1 ? "page" : "pages"}</span>
              <Status status={s.status} />
            </span>
          </button>

          {#if expanded[s.name]}
            <div class="border-t border-line px-3.5 py-3">
              <div class="mb-3 flex flex-wrap items-center gap-x-5 gap-y-1 text-[12px] text-faint">
                <span>Last sync: {s.last_refresh_at ? fmtDate(s.last_refresh_at) : "never"}</span>
                <span>Auto refresh: {s.refresh_interval || "manual"}</span>
                {#if s.type === "confluence"}
                  <span class="font-mono">{s.config?.base_url} · {s.config?.space}</span>
                  {#if s.config?.include_labels?.length}
                    <span>labels: {s.config.include_labels.join(", ")}</span>
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
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    {/if}
  </div>
{/if}
