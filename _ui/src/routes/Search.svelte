<script>
  // Code search supports local BM25 and semantic vectors. Docs search adds a
  // hybrid mode that fuses BM25 and semantic document ranks.
  import { onDestroy, onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { navigate } from "../lib/router.js";
  import { fmtDate } from "../lib/format.js";
  import Icon from "../lib/Icon.svelte";

  // Repo ids for the filter dropdown, loaded once. Capped so a huge fleet does
  // not build an enormous native <select>; beyond the cap the user searches all
  // repositories (the common case) or types the id via the query.
  const repoOptionCap = 500;
  let repoOptions = $state([]);
  let repoOptionsTruncated = $state(false);
  // Web-source collections for docs-search scoping (searched as "web:<name>").
  let sourceOptions = $state([]);
  // Namespaces for the namespace filter: [{ namespace, count, description }].
  let namespaceOptions = $state([]);

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
    try {
      namespaceOptions = (await api.namespaces()) || [];
    } catch {
      namespaceOptions = [];
    }
  }

  let q = $state("");
  // where encodes the docs search target: "" (everything), "repos" / "sources"
  // (whole namespace), a repo id, or "web:<name>". Code search only supports
  // repo ids, so switching to code resets non-repo selections.
  let repoFilter = $state("");
  // namespaceFilter scopes results to a single namespace; "" searches all.
  let namespaceFilter = $state("");
  let scope = $state("code");
  let codeMode = $state("normal");
  // Semantic is the default: on a large single-domain collection the BM25
  // arm has to score most of the corpus, and hybrid waits for it.
  let docsMode = $state("semantic");
  let results = $state(null); // null = not searched yet
  let total = $state(0);
  let page = $state(1);
  const perPage = 20;
  let pageCount = $derived(Math.max(1, Math.ceil(total / perPage)));
  let loading = $state(false);
  let error = $state("");
  let searchSeq = 0;
  // Controller for the in-flight request. A docs query over a large corpus can
  // run for a long time, so the user must be able to abort it rather than wait
  // on "Searching…"; aborting also frees the browser's connection.
  let searchAbort = null;

  function abortSearch() {
    searchAbort?.abort();
    searchAbort = null;
  }

  // cancelSearch is the explicit user action: stop the request and go back to
  // whatever was on screen before.
  function cancelSearch() {
    if (!loading) return;
    searchSeq++;
    abortSearch();
    loading = false;
  }

  async function search(nextPage = 1) {
    const query = q.trim();
    if (!query) return;
    const seq = ++searchSeq;
    const searchScope = scope;
    const searchMode = searchScope === "docs" ? docsMode : codeMode;
    abortSearch();
    const controller = new AbortController();
    searchAbort = controller;
    loading = true;
    error = "";
    try {
      // Map the where-selector onto the API params: the "repos"/"sources"
      // values become the docs scope param, everything else (repo id or
      // web:<name>) is a key. namespaceFilter is an orthogonal filter passed
      // through to both search kinds ("" = every namespace).
      const docsScope = repoFilter === "repos" || repoFilter === "sources" ? repoFilter : "";
      const key = docsScope ? "" : repoFilter;
      const opts = { signal: controller.signal };
      const response =
        searchScope === "docs"
          ? await api.searchDocs(query, key, 5, docsScope, namespaceFilter, searchMode, opts)
          : await api.searchCode(query, repoFilter, searchMode, nextPage, perPage, 0, namespaceFilter, opts);
      if (seq !== searchSeq) return;
      results = searchScope === "docs" ? (Array.isArray(response) ? response : []) : response?.results || [];
      total = searchScope === "docs" ? results.length : response?.total || 0;
      page = searchScope === "docs" ? 1 : response?.page || nextPage;
    } catch (e) {
      // A cancelled request already restored the UI; it is not an error.
      if (seq !== searchSeq || e?.name === "AbortError") return;
      error = e.message;
      results = [];
      total = 0;
    } finally {
      if (seq === searchSeq) {
        loading = false;
        searchAbort = null;
      }
    }
  }

  function resetResults() {
    searchSeq++;
    abortSearch();
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

  function searchPlaceholder() {
    if (scope === "code") {
      return codeMode === "normal" ? "Search code, symbols or paths…" : "Describe the code you are looking for…";
    }
    if (docsMode === "lexical") return "Search exact terms, Jira keys, error codes or titles…";
    if (docsMode === "semantic") return "Describe the documentation you are looking for…";
    return "Search documentation by meaning, terms or issue key…";
  }

  function docsModeHelp() {
    if (docsMode === "lexical")
      return "BM25 keyword search; best for exact identifiers and titles. Quote a phrase or use OR/NOT for full control.";
    if (docsMode === "semantic") return "Embedding search; best for concepts and paraphrased questions.";
    return "Fuses BM25 and semantic ranks. The most thorough mode, and the slowest on large collections.";
  }

  onMount(loadRepoOptions);
  // Leaving the page must not leave a long query holding a connection.
  onDestroy(abortSearch);
</script>

<div class="mb-3 flex flex-wrap items-center gap-2">
  <div class="inline-flex rounded-md border border-line bg-surface p-1" role="group" aria-label="Search target">
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
  <select class="input flex-1" bind:value={repoFilter} onchange={resetResults} aria-label="Search scope">
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
  {#if namespaceOptions.length > 0}
    <select
      class="input sm:basis-[180px]"
      bind:value={namespaceFilter}
      onchange={resetResults}
      aria-label="Namespace filter"
    >
      <option value="">all namespaces</option>
      {#each namespaceOptions as ns (ns.namespace)}
        <option value={ns.namespace}>{ns.namespace} ({ns.count})</option>
      {/each}
    </select>
  {/if}
</div>

<div class="mb-4 flex flex-col gap-2 sm:flex-row">
  <div class="relative flex-1">
    <span class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-faint">
      <Icon name="search" size={14} />
    </span>
    <input
      class="input w-full pl-8"
      placeholder={searchPlaceholder()}
      bind:value={q}
      onkeydown={(e) => {
        if (e.key === "Enter") search();
        else if (e.key === "Escape") cancelSearch();
      }}
    />
  </div>
  {#if scope === "code"}
    <select
      class="input sm:basis-[130px]"
      value={codeMode}
      onchange={(e) => {
        codeMode = e.currentTarget.value;
        resetResults();
      }}
      aria-label="Search mode"
    >
      <option value="normal">Normal</option>
      <option value="semantic">Semantic</option>
    </select>
  {:else}
    <select
      class="input sm:basis-[130px]"
      value={docsMode}
      onchange={(e) => {
        docsMode = e.currentTarget.value;
        resetResults();
      }}
      aria-label="Documentation search mode"
      title="Hybrid combines semantic and BM25 ranks. Lexical is best for exact keys, titles, and identifiers."
    >
      <option value="semantic">Semantic</option>
      <option value="lexical">Lexical (BM25)</option>
      <option value="hybrid">Hybrid</option>
    </select>
  {/if}
  <button class="btn btn-primary" onclick={() => search()} disabled={loading || !q.trim()}>
    {loading ? "Searching…" : "Search"}
  </button>
  {#if loading}
    <button class="btn" onclick={cancelSearch} title="Stop the running search">Cancel</button>
  {/if}
</div>

{#if scope === "docs"}
  <div class="-mt-2 mb-4 text-[11px] text-faint">{docsModeHelp()}</div>
{/if}

{#if results !== null && !loading}
  {#if results.length === 0 && !error}
    <div class="card p-6 text-center text-dim">No matches.</div>
  {:else}
    <div class="mb-2 flex items-center justify-between text-[12px] text-faint">
      <span>{total} {total === 1 ? "match" : "matches"}</span>
      {#if scope === "code" && codeMode === "normal" && pageCount > 1}
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
              <span class="ml-auto flex items-center gap-2 text-[11px] text-faint">
                {#if r.updated_at && !r.updated_at.startsWith("0001")}
                  <span title="last updated">{fmtDate(r.updated_at)}</span>
                {/if}
                <span title={docsMode === "hybrid" ? "Fused rank score; comparable only within this result list." : ""}>
                  {docsMode === "semantic" ? pct(r.score) : docsMode === "lexical" ? `BM25 ${r.score.toFixed(2)}` : `RRF ${r.score.toFixed(4)}`}
                </span>
              </span>
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
                {codeMode === "normal" && r.line ? `L${r.line}` : `L${r.start_line}–${r.end_line}`}
              </span>
              {#if r.symbol}
                <span class="rounded border border-line px-1.5 text-[11px] text-dim">{r.symbol}</span>
              {/if}
              <span class="ml-auto text-[11px] text-faint">
                {codeMode === "semantic" ? pct(r.score) : `BM25 ${r.score.toFixed(2)}`}
              </span>
            </div>
            <pre class="m-0 max-h-56 overflow-hidden px-3.5 py-2.5 font-mono text-[12px] leading-relaxed text-dim">{r.snippet}</pre>
          </button>
        {/if}
      {/each}
    </div>
    {#if scope === "code" && codeMode === "normal" && pageCount > 1}
      <div class="mt-4 flex items-center justify-center gap-3">
        <button class="btn btn-sm" disabled={page <= 1} onclick={() => search(page - 1)}>Previous</button>
        <span class="text-[12px] text-dim">{page} / {pageCount}</span>
        <button class="btn btn-sm" disabled={page >= pageCount} onclick={() => search(page + 1)}>Next</button>
      </div>
    {/if}
  {/if}
{/if}
