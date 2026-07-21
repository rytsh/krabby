<script>
  import { onMount, onDestroy } from "svelte";
  import { api } from "../lib/api.js";
  import { link } from "../lib/router.js";
  import Status from "../lib/Status.svelte";

  let repos = [];
  let error = "";
  let loading = true;
  let addUrl = "";
  let addBranch = "";
  let adding = false;
  let timer;

  async function load() {
    try {
      repos = await api.repos();
      error = "";
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  async function add() {
    if (!addUrl.trim()) return;
    adding = true;
    try {
      await api.addRepo(addUrl.trim(), addBranch.trim());
      addUrl = "";
      addBranch = "";
      await load();
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
      await load();
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
      await load();
    } catch (err) {
      error = err.message;
    }
  }

  function fmt(ts) {
    if (!ts || ts.startsWith("0001")) return "—";
    return new Date(ts).toLocaleString();
  }

  onMount(() => {
    load();
    // Poll so building/cloning repos update live without manual refresh.
    timer = setInterval(load, 4000);
  });
  onDestroy(() => clearInterval(timer));
</script>

<p class="text-dim">Tracked repositories and their knowledge-graph build status.</p>

<div class="card my-4 flex gap-2 p-3">
  <input
    class="input flex-1"
    placeholder="git URL (ssh or https)"
    bind:value={addUrl}
    on:keydown={(e) => e.key === "Enter" && add()}
  />
  <input class="input basis-[180px]" placeholder="branch (optional)" bind:value={addBranch} />
  <button class="btn btn-primary" on:click={add} disabled={adding || !addUrl.trim()}>
    {adding ? "Adding…" : "Add repo"}
  </button>
</div>

{#if error}
  <div class="err-box">{error}</div>
{/if}

<div class="card overflow-hidden">
  {#if loading}
    <div class="p-6 text-center text-dim">Loading…</div>
  {:else if repos.length === 0}
    <div class="p-6 text-center text-dim">No repositories tracked yet.</div>
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
        {#each repos as r}
          <tr class="hover:bg-surface-2">
            <td class="border-b border-line px-3 py-2.5">
              <a href={`/repos/${r.id}`} use:link class="font-mono text-[13px] hover:text-accent">{r.id}</a>
            </td>
            <td class="border-b border-line px-3 py-2.5"><Status status={r.status} /></td>
            <td class="border-b border-line px-3 py-2.5 font-mono text-[13px] text-faint">
              {r.last_commit ? r.last_commit.slice(0, 8) : "—"}
            </td>
            <td class="border-b border-line px-3 py-2.5 text-[13px] text-faint">{fmt(r.last_build_at)}</td>
            <td class="whitespace-nowrap border-b border-line px-3 py-2.5 text-right">
              <button class="btn btn-sm ml-1.5" on:click={(e) => refresh(r.id, e)}>Refresh</button>
              <button class="btn btn-sm btn-danger ml-1.5" on:click={(e) => remove(r.id, e)}>Remove</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
