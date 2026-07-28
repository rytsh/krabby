<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { link } from "../lib/router.js";
  import { invalidateOwners } from "../lib/repos.js";
  import { fmtDate } from "../lib/format.js";
  import Icon from "../lib/Icon.svelte";
  import Status from "../lib/Status.svelte";
  import { successToast } from "../lib/toast.js";

  let error = $state("");
  let addUrl = $state("");
  let addBranch = $state("");
  let addNamespace = $state("");
  // Per-repo overrides of the install-wide indexing/documentation settings,
  // offered at add time so a repo that does not fit the defaults is built
  // correctly on its first pass instead of being reindexed afterwards. Behind a
  // toggle: the common case is adding a plain source repository.
  let showAddAdvanced = $state(false);
  let addOverrides = $state(emptyOverrides());

  function emptyOverrides() {
    return {
      include: "",
      include_extra: "",
      exclude: "",
      graph_exclude: "",
      docs_prompt: "",
      docs_prompt_extra: "",
    };
  }

  function splitGlobs(v) {
    return v
      .split(",")
      .map((x) => x.trim())
      .filter(Boolean);
  }

  function overridesPayload(o) {
    const payload = {
      include: splitGlobs(o.include),
      include_extra: splitGlobs(o.include_extra),
      exclude: splitGlobs(o.exclude),
      graph_exclude: splitGlobs(o.graph_exclude),
      docs_prompt: o.docs_prompt.trim(),
      docs_prompt_extra: o.docs_prompt_extra.trim(),
    };
    const empty =
      !payload.include.length &&
      !payload.include_extra.length &&
      !payload.exclude.length &&
      !payload.graph_exclude.length &&
      !payload.docs_prompt &&
      !payload.docs_prompt_extra;

    return empty ? null : payload;
  }
  let adding = $state(false);

  // Server-side pagination + search + status filter. The API returns one page
  // at a time.
  let query = $state("");
  let status = $state("");
  let page = $state(1);
  const pageSize = 10;

  // Repo status values the backend can report (registry.Status*), plus web
  // sources reuse the same field. Keep in sync with lib/Status.svelte colors.
  const statusOptions = ["pending", "cloning", "building", "ready", "error"];

  let items = $state([]);
  let total = $state(0);
  let loaded = $state(false);
  let loading = $state(false);
  let loadSeq = 0;
  let debounceTimer;

  let totalPages = $derived(Math.max(1, Math.ceil(total / pageSize)));

  async function load() {
    const seq = ++loadSeq;
    loading = true;
    try {
      const res = await api.repos({ page, perPage: pageSize, q: query.trim(), status });
      if (seq !== loadSeq) return;
      items = res?.items || [];
      total = res?.total || 0;
      error = "";
    } catch (e) {
      if (seq !== loadSeq) return;
      error = e.message;
      items = [];
      total = 0;
    } finally {
      if (seq === loadSeq) {
        loading = false;
        loaded = true;
      }
    }
  }

  function setQuery(v) {
    query = v;
    page = 1;
    clearTimeout(debounceTimer);
    debounceTimer = setTimeout(load, 250);
  }

  function setStatus(v) {
    status = v;
    page = 1;
    load();
  }

  function goto(p) {
    page = Math.min(Math.max(1, p), totalPages);
    load();
  }

  // Reload the current page and refresh the sidebar owner tree after a mutation.
  async function reload() {
    await Promise.all([load(), invalidateOwners()]);
  }

  async function add() {
    if (!addUrl.trim()) return;
    adding = true;
    try {
      await api.addRepo(
        addUrl.trim(),
        addBranch.trim(),
        addNamespace.trim(),
        overridesPayload(addOverrides),
      );
      addUrl = "";
      addBranch = "";
      addNamespace = "";
      addOverrides = emptyOverrides();
      showAddAdvanced = false;
      error = "";
      page = 1;
      await reload();
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
      successToast("Refresh queued");
      await reload();
    } catch (err) {
      error = err.message;
    }
  }

  async function cancel(id, e) {
    e.preventDefault();
    e.stopPropagation();
    try {
      await api.cancelRepoJob(id);
      successToast("Cancel requested");
      await reload();
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
      // Stepping back a page if we just emptied the last one.
      if (items.length === 1 && page > 1) page -= 1;
      await reload();
    } catch (err) {
      error = err.message;
    }
  }

  onMount(load);
</script>

<p class="text-dim">Tracked repositories and their knowledge-graph build status.</p>

<div class="card my-4 flex flex-col gap-2 p-3">
  <div class="flex gap-2">
    <input
      class="input flex-1"
      placeholder="git URL (ssh or https)"
      bind:value={addUrl}
      onkeydown={(e) => e.key === "Enter" && add()}
    />
    <input class="input basis-[180px]" placeholder="branch (optional)" bind:value={addBranch} />
    <input class="input basis-[180px]" placeholder="namespace (optional)" bind:value={addNamespace} />
    <button class="btn" onclick={() => (showAddAdvanced = !showAddAdvanced)}>
      {showAddAdvanced ? "Hide overrides" : "Overrides"}
    </button>
    <button class="btn btn-primary" onclick={add} disabled={adding || !addUrl.trim()}>
      {adding ? "Adding…" : "Add repo"}
    </button>
  </div>

  {#if showAddAdvanced}
    <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Also index these (added to the defaults)
        <input class="input" placeholder="**/*.yaml, **/*.yml" bind:value={addOverrides.include_extra} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Index only these (replaces the defaults)
        <input class="input" placeholder="empty = built-in allowlist" bind:value={addOverrides.include} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Skip these
        <input class="input" placeholder="**/generated/**" bind:value={addOverrides.exclude} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Keep out of the knowledge graph
        <input class="input" placeholder="proto/, **/*.gen.go" bind:value={addOverrides.graph_exclude} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Replace the documentation prompt
        <input class="input" placeholder="empty = default prompt" bind:value={addOverrides.docs_prompt} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim sm:col-span-2">
        Extra documentation instructions
        <textarea
          class="input min-h-[70px]"
          placeholder="Environments are separate compose files; render a markdown table of service, image and version per environment."
          bind:value={addOverrides.docs_prompt_extra}
        ></textarea>
      </label>
      <p class="m-0 text-[12px] text-faint sm:col-span-2">
        These apply to a newly tracked repo only; an already tracked URL keeps its settings. Editable
        later from the repository page.
      </p>
    </div>
  {/if}
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
  <select
    class="input basis-[150px] capitalize"
    aria-label="Filter by status"
    value={status}
    onchange={(e) => setStatus(e.target.value)}
  >
    <option value="">All statuses</option>
    {#each statusOptions as s}
      <option value={s} class="capitalize">{s}</option>
    {/each}
  </select>
  <span class="whitespace-nowrap text-[13px] text-faint">
    {total} total
  </span>
</div>

<div class="card overflow-hidden">
  {#if !loaded}
    <div class="p-6 text-center text-dim">Loading…</div>
  {:else if total === 0 && !query.trim() && !status}
    <div class="p-6 text-center text-dim">No repositories tracked yet.</div>
  {:else if items.length === 0}
    <div class="p-6 text-center text-dim">
      No repositories match{query.trim() ? ` “${query}”` : ""}{status
        ? `${query.trim() ? " with" : " the"} status “${status}”`
        : ""}.
    </div>
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
        {#each items as r (r.id)}
          <tr class="hover:bg-surface-2">
            <td class="border-b border-line px-3 py-2.5">
              <a href={`/repos/${r.id}`} use:link class="font-mono text-[13px] hover:text-accent">{r.id}</a>
            </td>
            <td class="border-b border-line px-3 py-2.5">
              <Status status={r.status} />
              {#if r.running}
                <span class="ml-1.5 text-[11px] text-busy">({r.running})</span>
              {/if}
            </td>
            <td class="border-b border-line px-3 py-2.5 font-mono text-[13px] text-faint">
              {r.last_commit ? r.last_commit.slice(0, 8) : "—"}
            </td>
            <td class="border-b border-line px-3 py-2.5 text-[13px] text-faint">{fmtDate(r.last_build_at)}</td>
            <td class="whitespace-nowrap border-b border-line px-3 py-2.5 text-right">
              {#if r.running}
                <button class="btn btn-sm btn-danger ml-1.5" onclick={(e) => cancel(r.id, e)}>Cancel</button>
              {:else}
                <button class="btn btn-sm ml-1.5" onclick={(e) => refresh(r.id, e)}>Refresh</button>
              {/if}
              <button class="btn btn-sm btn-danger ml-1.5" onclick={(e) => remove(r.id, e)}>Remove</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>

    {#if totalPages > 1}
      <div class="flex min-h-8 items-center justify-between border-t border-line px-3 py-1">
        <span class="text-[11px] text-faint">
          {(page - 1) * pageSize + 1}–{Math.min(page * pageSize, total)} of {total}
        </span>
        <div class="flex items-center gap-1">
          <button
            class="btn inline-flex h-6 w-6 items-center justify-center !p-0 text-sm"
            aria-label="Previous page"
            disabled={page === 1 || loading}
            onclick={() => goto(page - 1)}
          >‹</button>
          <span class="min-w-12 text-center text-[11px] text-dim">{page} / {totalPages}</span>
          <button
            class="btn inline-flex h-6 w-6 items-center justify-center !p-0 text-sm"
            aria-label="Next page"
            disabled={page === totalPages || loading}
            onclick={() => goto(page + 1)}
          >›</button>
        </div>
      </div>
    {/if}
  {/if}
</div>
