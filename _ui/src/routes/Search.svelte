<script>
  // Normal BM25 search uses the local bw FTS index; semantic mode uses the code
  // vector index. Clicking a result opens the file at the matching line.
  import { api } from "../lib/api.js";
  import { navigate } from "../lib/router.js";
  import { repos } from "../lib/repos.js";
  import Icon from "../lib/Icon.svelte";

  let q = $state("");
  let repoFilter = $state("");
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
    const searchMode = mode;
    loading = true;
    error = "";
    try {
      const response = await api.searchCode(query, repoFilter, searchMode, nextPage, perPage);
      if (seq !== searchSeq) return;
      results = response?.results || [];
      total = response?.total || 0;
      page = response?.page || nextPage;
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
    navigate(`/repos/${r.repo}?file=${encodeURIComponent(r.path)}&line=${r.line || r.start_line || 1}`);
  }

  function pct(score) {
    return `${Math.round(score * 100)}%`;
  }
</script>

<div class="mb-4 flex flex-col gap-2 sm:flex-row">
  <div class="relative flex-1">
    <span class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-faint">
      <Icon name="search" size={14} />
    </span>
    <input
      class="input w-full pl-8"
      placeholder={mode === "normal" ? "Search code, symbols or paths…" : "Describe the code you are looking for…"}
      bind:value={q}
      onkeydown={(e) => e.key === "Enter" && search()}
    />
  </div>
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
  <select class="input sm:basis-[220px]" bind:value={repoFilter} onchange={resetResults}>
    <option value="">all repositories</option>
    {#each $repos as r (r.id)}
      <option value={r.id}>{r.id}</option>
    {/each}
  </select>
  <button class="btn btn-primary" onclick={search} disabled={loading || !q.trim()}>
    {loading ? "Searching…" : "Search"}
  </button>
</div>

{#if error}
  <div class="err-box">{error}</div>
{/if}

{#if results !== null && !loading}
  {#if results.length === 0 && !error}
    <div class="card p-6 text-center text-dim">No matches.</div>
  {:else}
    <div class="mb-2 flex items-center justify-between text-[12px] text-faint">
      <span>{total} {total === 1 ? "match" : "matches"}</span>
      {#if mode === "normal" && pageCount > 1}
        <span>Page {page} of {pageCount}</span>
      {/if}
    </div>
    <div class="flex flex-col gap-3">
      {#each results as r, i (i)}
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
      {/each}
    </div>
    {#if mode === "normal" && pageCount > 1}
      <div class="mt-4 flex items-center justify-center gap-3">
        <button class="btn btn-sm" disabled={page <= 1} onclick={() => search(page - 1)}>Previous</button>
        <span class="text-[12px] text-dim">{page} / {pageCount}</span>
        <button class="btn btn-sm" disabled={page >= pageCount} onclick={() => search(page + 1)}>Next</button>
      </div>
    {/if}
  {/if}
{/if}
