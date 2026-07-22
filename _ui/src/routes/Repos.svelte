<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { link } from "../lib/router.js";
  import { repos, reposLoaded, loadRepos } from "../lib/repos.js";
  import { fmtDate } from "../lib/format.js";
  import Icon from "../lib/Icon.svelte";
  import Status from "../lib/Status.svelte";

  let error = $state("");
  let addUrl = $state("");
  let addBranch = $state("");
  let adding = $state(false);

  // Search + pagination (client side: the API returns the full list).
  let query = $state("");
  let page = $state(1);
  const pageSize = 10;

  let filtered = $derived($repos.filter((r) => {
    const q = query.trim().toLowerCase();
    if (!q) return true;
    return r.id.toLowerCase().includes(q) || (r.url || "").toLowerCase().includes(q);
  }));
  let totalPages = $derived(Math.max(1, Math.ceil(filtered.length / pageSize)));
  // Clamp against filter shrink without writing state from an effect.
  let current = $derived(Math.min(page, totalPages));
  let paged = $derived(filtered.slice((current - 1) * pageSize, current * pageSize));

  function setQuery(v) {
    query = v;
    page = 1;
  }

  async function add() {
    if (!addUrl.trim()) return;
    adding = true;
    try {
      await api.addRepo(addUrl.trim(), addBranch.trim());
      addUrl = "";
      addBranch = "";
      error = "";
      await loadRepos();
    } catch (e) {
      error = e.message;
    } finally {
      adding = false;
    }
  }

  async function refresh(id, e) {
    e.preventDefault();
    e.stopPropagation();
    try {
      await api.refreshRepo(id);
      await loadRepos();
    } catch (err) {
      error = err.message;
    }
  }

  async function remove(id, e) {
    e.preventDefault();
    e.stopPropagation();
    if (!confirm(`Stop tracking ${id} and delete its clone?`)) return;
    try {
      await api.deleteRepo(id);
      await loadRepos();
    } catch (err) {
      error = err.message;
    }
  }

  onMount(loadRepos);
</script>

<p class="text-dim">Tracked repositories and their knowledge-graph build status.</p>

<div class="card my-4 flex gap-2 p-3">
  <input
    class="input flex-1"
    placeholder="git URL (ssh or https)"
    bind:value={addUrl}
    onkeydown={(e) => e.key === "Enter" && add()}
  />
  <input class="input basis-[180px]" placeholder="branch (optional)" bind:value={addBranch} />
  <button class="btn btn-primary" onclick={add} disabled={adding || !addUrl.trim()}>
    {adding ? "Adding…" : "Add repo"}
  </button>
</div>

<div class="mb-3 flex items-center gap-2">
  <div class="relative flex-1">
    <span class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-faint">
      <Icon name="search" size={14} />
    </span>
    <input
      class="input w-full pl-8"
      placeholder="Search repositories…"
      value={query}
      oninput={(e) => setQuery(e.target.value)}
    />
  </div>
  <span class="whitespace-nowrap text-[13px] text-faint">
    {filtered.length} of {$repos.length}
  </span>
</div>

<div class="card overflow-hidden">
  {#if !$reposLoaded}
    <div class="p-6 text-center text-dim">Loading…</div>
  {:else if $repos.length === 0}
    <div class="p-6 text-center text-dim">No repositories tracked yet.</div>
  {:else if filtered.length === 0}
    <div class="p-6 text-center text-dim">No repositories match “{query}”.</div>
  {:else}
    <table class="w-full border-collapse">
      <thead>
        <tr class="text-[13px] text-dim">
          <th class="border-b border-line px-3 py-2 text-left font-medium">Repository</th>
          <th class="border-b border-line px-3 py-2 text-left font-medium">Status</th>
          <th class="border-b border-line px-3 py-2 text-left font-medium">Commit</th>
          <th class="border-b border-line px-3 py-2 text-left font-medium">Last build</th>
          <th class="border-b border-line px-3 py-2"></th>
        </tr>
      </thead>
      <tbody>
        {#each paged as r (r.id)}
          <tr class="hover:bg-surface-2">
            <td class="border-b border-line px-3 py-2.5">
              <a href={`/repos/${r.id}`} use:link class="font-mono text-[13px] hover:text-accent">{r.id}</a>
            </td>
            <td class="border-b border-line px-3 py-2.5"><Status status={r.status} /></td>
            <td class="border-b border-line px-3 py-2.5 font-mono text-[13px] text-faint">
              {r.last_commit ? r.last_commit.slice(0, 8) : "—"}
            </td>
            <td class="border-b border-line px-3 py-2.5 text-[13px] text-faint">{fmtDate(r.last_build_at)}</td>
            <td class="whitespace-nowrap border-b border-line px-3 py-2.5 text-right">
              <button class="btn btn-sm ml-1.5" onclick={(e) => refresh(r.id, e)}>Refresh</button>
              <button class="btn btn-sm btn-danger ml-1.5" onclick={(e) => remove(r.id, e)}>Remove</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>

    {#if totalPages > 1}
      <div class="flex min-h-8 items-center justify-between border-t border-line px-3 py-1">
        <span class="text-[11px] text-faint">
          {(current - 1) * pageSize + 1}–{Math.min(current * pageSize, filtered.length)} of {filtered.length}
        </span>
        <div class="flex items-center gap-1">
          <button
            class="btn inline-flex h-6 w-6 items-center justify-center !p-0 text-sm"
            aria-label="Previous page"
            disabled={current === 1}
            onclick={() => (page = current - 1)}
          >‹</button>
          <span class="min-w-12 text-center text-[11px] text-dim">{current} / {totalPages}</span>
          <button
            class="btn inline-flex h-6 w-6 items-center justify-center !p-0 text-sm"
            aria-label="Next page"
            disabled={current === totalPages}
            onclick={() => (page = current + 1)}
          >›</button>
        </div>
      </div>
    {/if}
  {/if}
</div>
