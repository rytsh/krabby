<script>
  import { onDestroy } from "svelte";
  import { push } from "svelte-spa-router";
  import { link } from "../lib/router.js";
  import { api } from "../lib/api.js";
  import { createLatestRequest } from "../lib/async.js";
  import { successToast } from "../lib/toast.js";
  import { fmtDate } from "../lib/format.js";
  import MarkdownView from "../lib/MarkdownView.svelte";
  import {
    MEMORY_KINDS, MEMORY_STATUSES, memoryURL, statusTone, groupSpaces,
    createMemoryDraft, memoryPayload, formatEvidence,
  } from "../lib/memory.js";

  let { memoryId = "" } = $props();

  let spaces = $state([]);
  let selectedSpaces = $state([]);
  let kind = $state("");
  let status = $state("");
  let repo = $state("");
  let query = $state("");
  let searchMode = $state("");
  let result = $state({ items: [], total: 0, page: 1, per_page: 20 });
  let searchNote = $state("");
  let loading = $state(true);
  let error = $state("");

  let current = $state(null);
  let editing = $state(false);
  let creating = $state(false);
  let draft = $state(createMemoryDraft());
  let note = $state("");
  let busy = $state(false);
  let formError = $state("");
  let duplicate = $state(null);

  // The author is free text the user chooses; remember it in this browser so
  // it does not have to be typed for every memory.
  const AUTHOR_KEY = "krabby-memory-author";
  function savedAuthor() {
    try { return localStorage.getItem(AUTHOR_KEY) || ""; } catch { return ""; }
  }
  function rememberAuthor(name) {
    try { if (name) localStorage.setItem(AUTHOR_KEY, name); else localStorage.removeItem(AUTHOR_KEY); } catch { /* storage unavailable */ }
  }
  let alive = true;
  const listRequests = createLatestRequest();
  const detailRequests = createLatestRequest();

  let spaceGroups = $derived(groupSpaces(spaces));
  let filters = $derived({ space: selectedSpaces, kind, status, repo });

  async function loadSpaces() {
    try { spaces = await api.memorySpaces(); } catch { /* toast already shown */ }
  }

  async function loadList(page = 1) {
    const request = listRequests.nextAbortable();
    loading = true;
    error = "";
    searchNote = "";
    try {
      if (query.trim()) {
        const res = await api.searchMemories({ ...filters, q: query, mode: searchMode }, request.signal);
        if (!request.isLatest()) return;
        result = { items: res.results, total: res.results.length, page: 1, per_page: res.results.length || 20 };
        searchNote = `${res.mode} search${res.note ? ` — ${res.note}` : ""}`;
      } else {
        const data = await api.memories({ ...filters, page }, request.signal);
        if (request.isLatest()) result = data;
      }
    } catch (e) { if (request.isLatest() && e.name !== "AbortError") error = e.message; }
    finally { if (request.isLatest()) loading = false; }
  }

  async function loadDetail(id) {
    const request = detailRequests.nextAbortable();
    editing = false;
    if (!id) { current = null; return; }
    try {
      const m = await api.memory(id, request.signal);
      if (request.isLatest()) current = m;
    } catch (e) { if (request.isLatest() && e.name !== "AbortError") { current = null; error = e.message; } }
  }

  $effect(() => { void loadSpaces(); });
  $effect(() => { void filters; void loadList(); });
  $effect(() => { void loadDetail(memoryId); });
  onDestroy(() => { alive = false; listRequests.invalidate(); detailRequests.invalidate(); });

  function toggleSpace(space) {
    selectedSpaces = selectedSpaces.includes(space) ? selectedSpaces.filter((s) => s !== space) : [...selectedSpaces, space];
  }

  function startCreate() {
    creating = true;
    editing = false;
    duplicate = null;
    formError = "";
    draft = createMemoryDraft(null, savedAuthor());
    if (selectedSpaces.length === 1) draft.space = selectedSpaces[0];
  }

  function startEdit() {
    editing = true;
    creating = false;
    formError = "";
    note = "";
    draft = createMemoryDraft(current, savedAuthor());
  }

  async function save() {
    busy = true;
    formError = "";
    duplicate = null;
    try {
      const payload = memoryPayload(draft);
      rememberAuthor(payload.author);
      if (creating) {
        const res = await api.createMemory(payload);
        if (!alive) return;
        if (res.duplicate) { duplicate = res; return; }
        creating = false;
        successToast("Memory saved");
        await Promise.all([loadSpaces(), loadList()]);
        await push(memoryURL(res.memory.id));
      } else {
        // The memory keeps its original author; the editor's name goes into
        // the history entry for this change.
        const { author, ...fields } = payload;
        const updated = await api.updateMemory(current.id, { ...fields, author, expected_version: current.version, note });
        if (!alive) return;
        current = updated;
        editing = false;
        successToast("Memory updated");
        await Promise.all([loadSpaces(), loadList(result.page)]);
      }
    } catch (e) { if (alive) formError = e.message; }
    finally { if (alive) busy = false; }
  }

  async function confirmCurrent() {
    busy = true;
    try {
      current = await api.updateMemory(current.id, { expected_version: current.version, author: savedAuthor(), note: "reviewed against current code" });
      successToast("Memory confirmed and re-pinned");
      await loadList(result.page);
    } catch { /* toast already shown */ }
    finally { if (alive) busy = false; }
  }

  async function remove() {
    if (!confirm(`Forget "${current.title}"? Every agent loses it.`)) return;
    busy = true;
    try {
      await api.deleteMemory(current.id);
      if (!alive) return;
      successToast("Memory forgotten");
      await Promise.all([loadSpaces(), loadList(result.page)]);
      await push(memoryURL());
    } catch { /* toast already shown */ }
    finally { if (alive) busy = false; }
  }

  function evidenceLink(e) {
    const line = e.start_line ? `&line=${e.start_line}` : "";
    return `/repos/${e.repo}?file=${encodeURIComponent(e.path)}${line}`;
  }
</script>

<div class="mb-4 flex flex-wrap items-start justify-between gap-3">
  <div>
    <h2 class="mb-1 text-[15px] font-semibold">Shared memory</h2>
    <p class="text-dim">Durable Markdown notes agents and people share across sessions. Spaces group them — <code>default</code> for team knowledge, <code>agent:&lt;name&gt;</code> for an agent's own notes — and everyone can read every space. Notes pinned to code are re-checked after each repository refresh.</p>
  </div>
  <div class="flex shrink-0 gap-2">
    <a class="btn" href={api.memoryExportURL(filters)} download>Export Markdown</a>
    <button class="btn btn-primary" onclick={startCreate}>New memory</button>
  </div>
</div>

<div class="flex min-w-0 flex-col gap-4 lg:flex-row">
  <aside class="card shrink-0 space-y-3 p-3 lg:w-56">
    {#each spaceGroups as group (group.label)}
      <div>
        <h3 class="mb-1 text-[11px] font-semibold uppercase tracking-wide text-faint">{group.label}</h3>
        <ul>
          {#each group.items as s (s.space)}
            <li>
              <button class="flex w-full items-center justify-between rounded px-2 py-1 text-left text-[13px] hover:bg-surface-2 {selectedSpaces.includes(s.space) ? 'bg-surface-2 font-semibold' : ''}" aria-pressed={selectedSpaces.includes(s.space)} onclick={() => toggleSpace(s.space)}>
                <span class="truncate font-mono">{s.space}</span>
                <span class="text-[11px] text-faint">{s.count}{#if s.status?.stale || s.status?.orphaned}<span class="ml-1 text-warn" title="stale or orphaned">●</span>{/if}</span>
              </button>
            </li>
          {/each}
        </ul>
      </div>
    {/each}
    {#if selectedSpaces.length}<button class="btn btn-sm w-full" onclick={() => { selectedSpaces = []; }}>All spaces</button>{/if}
  </aside>

  <div class="min-w-0 flex-1">
    <form class="mb-3 flex flex-wrap gap-2" onsubmit={(event) => { event.preventDefault(); loadList(); }}>
      <input class="input min-w-48 flex-1" aria-label="Search memories" bind:value={query} placeholder="Search by meaning or keywords…" />
      <select class="input w-32" aria-label="Search mode" bind:value={searchMode}><option value="">auto</option><option value="hybrid">hybrid</option><option value="semantic">semantic</option><option value="lexical">lexical</option></select>
      <select class="input w-36" aria-label="Kind" bind:value={kind}><option value="">all kinds</option>{#each MEMORY_KINDS as k (k)}<option value={k}>{k}</option>{/each}</select>
      <select class="input w-36" aria-label="Status" bind:value={status}><option value="">current</option>{#each MEMORY_STATUSES as s (s)}<option value={s}>{s}</option>{/each}<option value="*">everything</option></select>
      <input class="input w-56" aria-label="Repository" bind:value={repo} placeholder="repo id" onchange={() => loadList()} />
      <button class="btn" type="submit">Search</button>
    </form>
    {#if searchNote}<p class="mb-2 text-[12px] text-faint">{searchNote}</p>{/if}
    {#if error}<p class="mb-3 text-err" role="alert">{error}</p>{/if}

    {#if creating || editing}
      <form class="card mb-4 space-y-3 p-4" onsubmit={(event) => { event.preventDefault(); save(); }}>
        <h3 class="text-[15px] font-semibold">{creating ? "New memory" : `Edit “${current.title}”`}</h3>
        <div class="grid gap-3 sm:grid-cols-4">
          <label class="text-[12px] text-dim">{creating ? "Author" : "Edited by"} <span class="text-faint">(optional)</span><input class="input mt-1 w-full" bind:value={draft.author} maxlength="128" placeholder="name, team or agent" /></label>
          <label class="text-[12px] text-dim">Space<input class="input mt-1 w-full font-mono" bind:value={draft.space} placeholder="default" list="memory-spaces" /></label>
          <label class="text-[12px] text-dim">Kind<select class="input mt-1 w-full" bind:value={draft.kind}>{#each MEMORY_KINDS as k (k)}<option value={k}>{k}</option>{/each}</select></label>
          <label class="text-[12px] text-dim">Expires<input class="input mt-1 w-full" type="date" bind:value={draft.expires_at} /></label>
        </div>
        <datalist id="memory-spaces">{#each spaces as s (s.space)}<option value={s.space}></option>{/each}</datalist>
        <label class="block text-[12px] text-dim">Title<input class="input mt-1 w-full" bind:value={draft.title} maxlength="200" required /></label>
        <label class="block text-[12px] text-dim">Content (Markdown)<textarea class="input mt-1 min-h-40 w-full resize-y font-mono text-[13px]" bind:value={draft.content} required></textarea></label>
        <label class="block text-[12px] text-dim">Tags (comma separated)<input class="input mt-1 w-full" bind:value={draft.tags_text} /></label>
        <label class="block text-[12px] text-dim">Evidence, one per line: <code>repo:path#L10-20</code>, <code>repo:path</code> or <code>repo:@Symbol</code><textarea class="input mt-1 min-h-16 w-full resize-y font-mono text-[12px]" bind:value={draft.evidence_text}></textarea></label>
        {#if editing}<label class="block text-[12px] text-dim">Change note (kept in history)<input class="input mt-1 w-full" bind:value={note} /></label>{/if}
        {#if duplicate}
          <p class="text-[13px] text-warn" role="alert">{duplicate.note} <a class="underline" href={memoryURL(duplicate.memory.id)} use:link onclick={() => { creating = false; }}>Open “{duplicate.memory.title}”</a></p>
        {/if}
        {#if formError}<p class="text-err" role="alert">{formError}</p>{/if}
        <div class="flex gap-2"><button class="btn btn-primary" type="submit" disabled={busy}>Save</button><button class="btn" type="button" disabled={busy} onclick={() => { creating = false; editing = false; }}>Cancel</button></div>
      </form>
    {/if}

    {#if current && !editing}
      {@const tone = statusTone(current.status)}
      <article class="card mb-4">
        <header class="flex flex-wrap items-start justify-between gap-2 border-b border-line px-4 py-3">
          <div class="min-w-0">
            <p class="text-[11px] text-faint"><span class="font-mono">{current.space}</span> · {current.kind} · v{current.version}</p>
            <h3 class="mt-1 text-[15px] font-semibold">{current.title}</h3>
            <p class="mt-1 text-[12px] text-faint">Created {fmtDate(current.created_at)}{#if current.author} by {current.author}{/if} · updated {fmtDate(current.updated_at)}{#if current.expires_at} · expires {fmtDate(current.expires_at)}{/if}</p>
          </div>
          <div class="flex flex-wrap items-center gap-2">
            <span class="inline-flex items-center gap-1.5 rounded-full border border-line px-2 py-0.5 text-[11px] text-dim"><span class="h-1.5 w-1.5 rounded-full {tone === 'ok' ? 'bg-ok' : tone === 'warn' ? 'bg-warn' : tone === 'err' ? 'bg-err' : 'bg-faint'}" aria-hidden="true"></span>{current.status}</span>
            {#if current.status === "stale" || current.status === "orphaned"}<button class="btn btn-sm" disabled={busy} onclick={confirmCurrent} title="Re-pin evidence to the current code">Still true</button>{/if}
            <a class="btn btn-sm" href={api.memoryMarkdownURL(current.id)} target="_blank" rel="noopener">Raw .md</a>
            <button class="btn btn-sm" disabled={busy} onclick={startEdit}>Edit</button>
            <button class="btn btn-sm btn-danger" disabled={busy} onclick={remove}>Forget</button>
            <a class="btn btn-sm" href={memoryURL()} use:link aria-label="Close">✕</a>
          </div>
        </header>
        {#if current.status === "stale"}<p class="border-b border-line bg-surface-2 px-4 py-2 text-[12px] text-warn">The code this memory is pinned to has changed since it was written. Check it, then edit or confirm.</p>{/if}
        {#if current.status === "orphaned"}<p class="border-b border-line bg-surface-2 px-4 py-2 text-[12px] text-err">A pinned file or repository no longer exists.</p>{/if}
        <MarkdownView markdown={current.content} />
        {#if current.tags.length}<p class="flex flex-wrap gap-1 px-4 pb-3">{#each current.tags as tag (tag)}<span class="rounded-full border border-line px-2 py-0.5 text-[11px] text-dim">#{tag}</span>{/each}</p>{/if}
        {#if current.evidence.length}
          <section class="border-t border-line px-4 py-3 text-[12px]">
            <h4 class="mb-2 font-semibold">Evidence</h4>
            <ul class="space-y-1">
              {#each current.evidence as e, i (i)}
                <li class="flex flex-wrap items-center gap-2 font-mono">
                  <span class="h-1.5 w-1.5 rounded-full {e.status === 'ok' ? 'bg-ok' : e.status === 'changed' ? 'bg-warn' : 'bg-err'}" title={e.status}></span>
                  <a class="break-all hover:underline" href={evidenceLink(e)} use:link>{formatEvidence(e)}</a>
                  {#if e.commit}<span class="text-faint">@ {e.commit.slice(0, 10)}</span>{/if}
                </li>
              {/each}
            </ul>
          </section>
        {/if}
        <details class="border-t border-line px-4 py-3 text-[12px]" open>
          <summary class="cursor-pointer font-semibold">History ({current.history.length})</summary>
          <ol class="mt-2 space-y-1">
            {#each [...current.history].reverse() as ev, i (i)}
              <li><span class="text-faint">{fmtDate(ev.at)}</span> · <strong>{ev.action}</strong>{#if ev.actor} by {ev.actor}{/if}{#if ev.note} — {ev.note}{/if}</li>
            {/each}
          </ol>
        </details>
      </article>
    {/if}

    {#if loading}<p class="text-dim">Loading…</p>
    {:else}
      <ul class="space-y-2">
        {#each result.items as m (m.id)}
          {@const tone = statusTone(m.status)}
          <li>
            <a class="card block p-3 transition-colors hover:bg-surface-2 {current?.id === m.id ? 'ring-1 ring-line' : ''}" href={memoryURL(m.id)} use:link>
              <div class="flex items-center justify-between gap-2 text-[11px] text-faint">
                <span><span class="font-mono">{m.space}</span> · {m.kind}{#if m.evidence?.length} · {m.evidence.length} pin{m.evidence.length > 1 ? "s" : ""}{/if}</span>
                <span class="inline-flex items-center gap-1.5"><span class="h-1.5 w-1.5 rounded-full {tone === 'ok' ? 'bg-ok' : tone === 'warn' ? 'bg-warn' : tone === 'err' ? 'bg-err' : 'bg-faint'}" aria-hidden="true"></span>{m.status} · {fmtDate(m.updated_at)}</span>
              </div>
              <h4 class="mt-1 text-[14px] font-semibold">{m.title}</h4>
              <p class="mt-1 line-clamp-2 text-[13px] text-dim">{m.content}</p>
            </a>
          </li>
        {:else}
          <li class="card p-6 text-center text-dim">No memories here yet. Agents add them through the <code>krabby-memory</code> MCP server, or use “New memory”.</li>
        {/each}
      </ul>
      {#if !query.trim()}
        <div class="mt-4 flex items-center justify-between text-[12px] text-dim"><span>{result.total} memories · page {result.page}</span><div class="flex gap-2"><button class="btn btn-sm" disabled={result.page <= 1} onclick={() => loadList(result.page - 1)}>Previous</button><button class="btn btn-sm" disabled={result.page * result.per_page >= result.total} onclick={() => loadList(result.page + 1)}>Next</button></div></div>
      {/if}
    {/if}
  </div>
</div>
