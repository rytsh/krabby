<script>
  // Code search supports local BM25 and semantic code vectors. Docs search uses
  // the generated-document vector index and returns bounded excerpts.
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { navigate } from "../lib/router.js";
  import Icon from "../lib/Icon.svelte";

  // Repo ids for the filter dropdown, loaded once. Capped so a huge fleet does
  // not build an enormous native <select>; beyond the cap the user searches all
  // repositories (the common case) or types the id via the query.
  const repoOptionCap = 500;
  let repoOptions = $state([]);
  let repoOptionsTruncated = $state(false);
  // Web-source collections for docs-search scoping (searched as "web:<name>").
  let sourceOptions = $state([]);

  async function loadRepoOptions() {
    try {
      const res = await api.repos({ page: 1, perPage: repoOptionCap });
      repoOptions = (res?.items || []).map((r) => r.id);
      repoOptionsTruncated = (res?.total || 0) > repoOptions.length;
    } catch {
      repoOptions = [];
    }
    try {
      sourceOptions = ((await api.sources()) || []).map((s) => s.name);
    } catch {
      sourceOptions = [];
    }
  }

  let q = $state("");
  // where encodes the docs search target: "" (everything), "repos" / "sources"
  // (whole namespace), a repo id, or "web:<name>". Code search only supports
  // repo ids, so switching to code resets non-repo selections.
  let repoFilter = $state("");
  let scope = $state("code");
  let mode = $state("normal");
  let results = $state(null); // null = not searched yet
  let total = $state(0);
  let page = $state(1);
  const perPage = 20;
  let pageCount = $derived(Math.max(1, Math.ceil(total / perPage)));
  let loading = $state(false);
  let error = $state("");
  let searchSeq = 0;

  async function search(nextPage = 1) {
    const query = q.trim();
    if (!query) return;
    const seq = ++searchSeq;
    const searchScope = scope;
    const searchMode = mode;
    loading = true;
    error = "";
    try {
      // Map the where-selector onto the API params: namespace values become
      // the scope param, everything else (repo id or web:<name>) is a key.
      const namespace = repoFilter === "repos" || repoFilter === "sources" ? repoFilter : "";
      const key = namespace ? "" : repoFilter;
      const response =
        searchScope === "docs"
          ? await api.searchDocs(query, key, 5, namespace)
          : await api.searchCode(query, repoFilter, searchMode, nextPage, perPage);
      if (seq !== searchSeq) return;
      results = searchScope === "docs" ? (Array.isArray(response) ? response : []) : response?.results || [];
      total = searchScope === "docs" ? results.length : response?.total || 0;
      page = searchScope === "docs" ? 1 : response?.page || nextPage;
    } catch (e) {
      if (seq !== searchSeq) return;
      error = e.message;
      results = [];
      total = 0;
    } finally {
      if (seq === searchSeq) loading = false;
    }
  }

  function resetResults() {
    searchSeq++;
    results = null;
    total = 0;
    page = 1;
    error = "";
    loading = false;
  }

  function open(r) {
    if (scope === "docs") {
      // Web-source hits open the synced markdown on the Sources page.
      if (r.repo.startsWith("web:")) {
        navigate(`/sources/${r.repo.slice(4)}?doc=${encodeURIComponent(r.path)}`);
        return;
      }
      navigate(`/repos/${r.repo}?doc=${encodeURIComponent(r.path)}`);
      return;
    }
    navigate(`/repos/${r.repo}?file=${encodeURIComponent(r.path)}&line=${r.line || r.start_line || 1}`);
  }

  function pct(score) {
    return `${Math.round(score * 100)}%`;
  }

  function docExcerpt(content) {
    const text = (content || "").trim();
    return text.length > 700 ? `${text.slice(0, 700)}…` : text;
  }

  onMount(loadRepoOptions);
</script>

<div class="mb-3 inline-flex rounded-md border border-line bg-surface p-1" role="group" aria-label="Search target">
  <button
    class="view-toggle px-3 py-1"
    class:view-toggle-active={scope === "code"}
    onclick={() => {
      scope = "code";
      // Code search only understands repo ids; drop docs-only selections.
      if (repoFilter === "repos" || repoFilter === "sources" || repoFilter.startsWith("web:")) repoFilter = "";
      resetResults();
    }}>Code</button
  >
  <button
    class="view-toggle px-3 py-1"
    class:view-toggle-active={scope === "docs"}
    onclick={() => {
      scope = "docs";
      resetResults();
    }}>Docs</button
  >
</div>
{#if scope === "docs"}
  <span class="ml-2 text-[11px] text-faint">searches generated repo docs and synced web sources</span>
{/if}

<div class="mb-4 flex flex-col gap-2 sm:flex-row">
  <div class="relative flex-1">
    <span class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-faint">
      <Icon name="search" size={14} />
    </span>
    <input
      class="input w-full pl-8"
      placeholder={scope === "docs"
        ? "Describe the documentation you are looking for…"
        : mode === "normal"
          ? "Search code, symbols or paths…"
          : "Describe the code you are looking for…"}
      bind:value={q}
      onkeydown={(e) => e.key === "Enter" && search()}
    />
  </div>
  {#if scope === "code"}
    <select
      class="input sm:basis-[130px]"
      value={mode}
      onchange={(e) => {
        mode = e.currentTarget.value;
        resetResults();
      }}
      aria-label="Search mode"
    >
      <option value="normal">Normal</option>
      <option value="semantic">Semantic</option>
    </select>
  {:else}
    <div class="flex items-center rounded-md border border-line bg-surface px-3 text-[12px] text-faint sm:basis-[130px]">
      Semantic
    </div>
  {/if}
  <select class="input sm:basis-[240px]" bind:value={repoFilter} onchange={resetResults} aria-label="Search scope">
    {#if scope === "docs"}
      <option value="">everywhere</option>
      <option value="repos">all repositories</option>
      <option value="sources">all web sources</option>
      {#if sourceOptions.length > 0}
        <optgroup label="Web sources">
          {#each sourceOptions as name (name)}
            <option value={`web:${name}`}>web:{name}</option>
          {/each}
        </optgroup>
      {/if}
      <optgroup label="Repositories">
        {#each repoOptions as id (id)}
          <option value={id}>{id}</option>
        {/each}
      </optgroup>
    {:else}
      <option value="">all repositories</option>
      {#each repoOptions as id (id)}
        <option value={id}>{id}</option>
      {/each}
    {/if}
    {#if repoOptionsTruncated}
      <option disabled>… more (search all repositories)</option>
    {/if}
  </select>
  <button class="btn btn-primary" onclick={search} disabled={loading || !q.trim()}>
    {loading ? "Searching…" : "Search"}
  </button>
</div>

{#if results !== null && !loading}
  {#if results.length === 0 && !error}
    <div class="card p-6 text-center text-dim">No matches.</div>
  {:else}
    <div class="mb-2 flex items-center justify-between text-[12px] text-faint">
      <span>{total} {total === 1 ? "match" : "matches"}</span>
      {#if scope === "code" && mode === "normal" && pageCount > 1}
        <span>Page {page} of {pageCount}</span>
      {/if}
    </div>
    <div class="flex flex-col gap-3">
      {#each results as r, i (i)}
        {#if scope === "docs"}
          <button class="card block w-full cursor-pointer overflow-hidden text-left transition-colors hover:border-accent" onclick={() => open(r)}>
            <div class="flex items-center gap-2 border-b border-line bg-surface-2/50 px-3.5 py-2">
              <span class="truncate text-[13px] font-medium text-fg">{r.title || r.path}</span>
              <span class="font-mono text-[11px] text-faint">{r.repo} / {r.path}</span>
              <span class="ml-auto text-[11px] text-faint">{pct(r.score)}</span>
            </div>
            <pre class="m-0 max-h-56 overflow-hidden whitespace-pre-wrap px-3.5 py-2.5 font-mono text-[12px] leading-relaxed text-dim">{docExcerpt(r.excerpt)}</pre>
          </button>
        {:else}
          <button class="card block w-full cursor-pointer overflow-hidden text-left transition-colors hover:border-accent" onclick={() => open(r)}>
            <div class="flex items-center gap-2 border-b border-line bg-surface-2/50 px-3.5 py-2">
              <span class="font-mono text-[12.5px] text-fg">{r.repo}</span>
              <span class="text-faint">/</span>
              <span class="truncate font-mono text-[12.5px] text-dim">{r.path}</span>
              <span class="font-mono text-[11px] text-faint">
                {mode === "normal" && r.line ? `L${r.line}` : `L${r.start_line}–${r.end_line}`}
              </span>
              {#if r.symbol}
                <span class="rounded border border-line px-1.5 text-[11px] text-dim">{r.symbol}</span>
              {/if}
              <span class="ml-auto text-[11px] text-faint">
                {mode === "semantic" ? pct(r.score) : `BM25 ${r.score.toFixed(2)}`}
              </span>
            </div>
            <pre class="m-0 max-h-56 overflow-hidden px-3.5 py-2.5 font-mono text-[12px] leading-relaxed text-dim">{r.snippet}</pre>
          </button>
        {/if}
      {/each}
    </div>
    {#if scope === "code" && mode === "normal" && pageCount > 1}
      <div class="mt-4 flex items-center justify-center gap-3">
        <button class="btn btn-sm" disabled={page <= 1} onclick={() => search(page - 1)}>Previous</button>
        <span class="text-[12px] text-dim">{page} / {pageCount}</span>
        <button class="btn btn-sm" disabled={page >= pageCount} onclick={() => search(page + 1)}>Next</button>
      </div>
    {/if}
  {/if}
{/if}
