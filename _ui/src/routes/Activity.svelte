<script>
  import { onDestroy, onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { link } from "../lib/router.js";
  import Icon from "../lib/Icon.svelte";

  // Central work-queue snapshot: { limit, running, pending, tasks: [...] }.
  let snap = $state({ limit: 0, running: 0, pending: 0, tasks: [] });
  // Per-step detail for running repos: [{ id, running, status }].
  let active = $state([]);
  let repoCount = $state(0);
  let loaded = $state(false);

  // Human labels for the fine-grained pipeline steps surfaced per running repo.
  const stepMeta = {
    sync: "Syncing repository",
    graph: "Building knowledge graph",
    docs: "Generating documentation",
    docs_index: "Indexing documentation",
    code_index: "Indexing source code",
  };

  const kindLabel = {
    refresh: "Refresh",
    generate: "Generate",
    reindex: "Reindex",
    websync: "Web sync",
  };

  const stateMeta = {
    running: { label: "Running", text: "text-busy", dot: "bg-busy" },
    queued: { label: "Queued", text: "text-warn", dot: "bg-warn" },
    done: { label: "Done", text: "text-ok", dot: "bg-ok" },
    error: { label: "Error", text: "text-err", dot: "bg-err" },
    canceled: { label: "Canceled", text: "text-faint", dot: "bg-faint" },
  };

  // Map repo id -> comma-joined running steps, to enrich running tasks.
  let stepsById = $derived(Object.fromEntries(active.map((r) => [r.id, r.running])));

  function steps(id) {
    const s = stepsById[id];
    if (!s) return [];
    return s.split(",").map((step) => stepMeta[step] || step);
  }

  // Running first (oldest first = longest running), then queued FIFO.
  let live = $derived(
    (snap.tasks || [])
      .filter((t) => t.state === "running" || t.state === "queued")
      .sort((a, b) => {
        const rank = (s) => (s === "running" ? 0 : 1);
        return rank(a.state) - rank(b.state) || a.seq - b.seq;
      }),
  );
  let recent = $derived(
    (snap.tasks || []).filter((t) => ["done", "error", "canceled"].includes(t.state)).sort((a, b) => b.seq - a.seq),
  );

  const pageSize = 8;
  let livePage = $state(1);
  let liveTotalPages = $derived(Math.max(1, Math.ceil(live.length / pageSize)));
  let currentLivePage = $derived(Math.min(livePage, liveTotalPages));
  let visibleLive = $derived(live.slice((currentLivePage - 1) * pageSize, currentLivePage * pageSize));

  function fmtDur(ms) {
    ms = Math.max(0, ms);
    if (ms < 1000) return `${Math.round(ms)}ms`;
    const s = Math.round(ms / 1000);
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m ${s % 60}s`;
    const h = Math.floor(m / 60);
    return `${h}h ${m % 60}m`;
  }
  const ms = (t) => (t ? new Date(t).getTime() : 0);
  function timing(task) {
    if (task.state === "running") return `running for ${fmtDur(Date.now() - ms(task.started_at))}`;
    if (task.state === "queued") return `waiting ${fmtDur(Date.now() - ms(task.enqueued_at))}`;
    if (task.started_at && task.ended_at) return `took ${fmtDur(ms(task.ended_at) - ms(task.started_at))}`;
    return "";
  }

  function displayId(id) {
    if (id === "*") return "all repositories & sources";
    return id;
  }
  function repoHref(task) {
    if (task.id === "*" || task.id.startsWith("web:")) return null;
    return `/repos/${task.id}`;
  }

  let refreshing = $state(false);
  const LIVE_INTERVAL_KEY = "krabby-activity-live-interval";
  const liveIntervals = [0, 4_000, 10_000, 30_000, 60_000];
  const savedLiveInterval = Number(localStorage.getItem(LIVE_INTERVAL_KEY));
  let liveInterval = $state(liveIntervals.includes(savedLiveInterval) ? savedLiveInterval : 4_000);
  let timer;

  async function refresh() {
    refreshing = true;
    try {
      const [tasks, activeList, page] = await Promise.all([
        api.tasks(),
        api.activeRepos(),
        api.repos({ page: 1, perPage: 1 }),
      ]);
      snap = tasks && Array.isArray(tasks.tasks) ? tasks : { limit: 0, running: 0, pending: 0, tasks: [] };
      active = Array.isArray(activeList) ? activeList : [];
      repoCount = page?.total || 0;
    } finally {
      refreshing = false;
      loaded = true;
    }
  }

  // In-flight action guard so a task's buttons disable while its request runs.
  let busySeq = $state(0);

  async function bump(task) {
    busySeq = task.seq;
    try {
      const s = await api.bumpTask(task.seq);
      if (s && Array.isArray(s.tasks)) snap = s;
      else await refresh();
    } catch {
      // errorToast already surfaced it; re-sync so stale rows don't linger.
      await refresh();
    } finally {
      busySeq = 0;
    }
  }

  async function cancelQueued(task) {
    busySeq = task.seq;
    try {
      const s = await api.cancelTask(task.seq);
      if (s && Array.isArray(s.tasks)) snap = s;
      else await refresh();
    } catch {
      await refresh();
    } finally {
      busySeq = 0;
    }
  }

  async function cancelRunning(task) {
    if (task.id === "*" || task.id.startsWith("web:")) return;
    busySeq = task.seq;
    try {
      await api.cancelRepoJob(task.id);
    } catch {
      // ignore; toast shown
    } finally {
      busySeq = 0;
      await refresh();
    }
  }

  let savingLimit = $state(false);
  async function changeConcurrency(value) {
    const n = Number(value);
    if (!Number.isFinite(n) || n < 1) return;
    savingLimit = true;
    try {
      const s = await api.setTaskConcurrency(n);
      if (s && Array.isArray(s.tasks)) snap = s;
      else await refresh();
    } catch {
      await refresh();
    } finally {
      savingLimit = false;
    }
  }

  // A running repo job can be cancelled; the aggregate "*" and web syncs cannot.
  function canCancelRunning(task) {
    return task.id !== "*" && !task.id.startsWith("web:");
  }

  function setLiveInterval(value) {
    liveInterval = Number(value);
    localStorage.setItem(LIVE_INTERVAL_KEY, String(liveInterval));
    clearInterval(timer);
    timer = undefined;
    if (liveInterval > 0) {
      refresh();
      timer = setInterval(refresh, liveInterval);
    }
  }

  onMount(() => {
    refresh();
    if (liveInterval > 0) timer = setInterval(refresh, liveInterval);
  });
  onDestroy(() => clearInterval(timer));
</script>

<div class="mb-5 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
  <div>
    <p class="m-0 max-w-2xl text-dim">
      Live background work. Every task (repository refresh/generate, web-source sync, reindex) runs through one central
      queue whose concurrency limit — configurable in Settings — caps how many run at once.
    </p>
  </div>
  <div class="flex items-center gap-2">
    <label class="inline-flex items-center gap-1.5 text-[13px] text-dim">
      Live
      <select
        class="input !h-8 !w-auto !py-0 text-[12px]"
        value={liveInterval}
        onchange={(e) => setLiveInterval(e.currentTarget.value)}
        aria-label="Activity live refresh interval"
      >
        <option value={0}>Off</option>
        <option value={4000}>4s</option>
        <option value={10000}>10s</option>
        <option value={30000}>30s</option>
        <option value={60000}>60s</option>
      </select>
    </label>
    <button class="btn btn-sm inline-flex items-center gap-1.5" onclick={refresh} disabled={refreshing}>
      <Icon name="refresh" size={13} />
      {refreshing ? "Refreshing…" : "Refresh"}
    </button>
  </div>
</div>

<div class="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
  <div class="card px-4 py-3">
    <div class="font-mono text-2xl font-semibold leading-none text-busy">{snap.running}</div>
    <div class="mt-1.5 text-[12px] text-faint">Running</div>
  </div>
  <div class="card px-4 py-3">
    <div class="font-mono text-2xl font-semibold leading-none {snap.pending ? 'text-warn' : ''}">{snap.pending}</div>
    <div class="mt-1.5 text-[12px] text-faint">Queued</div>
  </div>
  <div class="card px-4 py-3">
    <div class="flex items-center gap-2">
      <input
        type="number"
        min="1"
        class="input !h-8 !w-16 !py-0 font-mono text-lg font-semibold"
        value={snap.limit}
        disabled={savingLimit}
        onchange={(e) => changeConcurrency(e.currentTarget.value)}
        aria-label="Queue concurrency limit"
      />
      {#if savingLimit}
        <span class="text-[11px] text-faint">saving…</span>
      {/if}
    </div>
    <div class="mt-1.5 text-[12px] text-faint">Concurrency limit</div>
  </div>
  <div class="card px-4 py-3">
    <div class="font-mono text-2xl font-semibold leading-none">{repoCount}</div>
    <div class="mt-1.5 text-[12px] text-faint">Tracked repositories</div>
  </div>
</div>

{#if !loaded}
  <div class="card p-8 text-center text-dim">Loading activity…</div>
{:else if live.length === 0 && recent.length === 0}
  <div class="card flex min-h-44 flex-col items-center justify-center border-dashed px-6 text-center">
    <div class="font-medium">No background tasks</div>
    <div class="mt-1 text-[13px] text-faint">Refresh a repository or generate an artifact to see it queued here.</div>
  </div>
{:else}
  {#if live.length > 0}
    <div class="mb-1 text-[11px] font-medium uppercase tracking-wider text-faint">
      Running &amp; queued ({live.length})
    </div>
    <div class="flex flex-col gap-2">
      {#each visibleLive as task (task.seq)}
        {@const meta = stateMeta[task.state] || stateMeta.queued}
        {@const href = repoHref(task)}
        <div
          class="card relative grid overflow-hidden sm:grid-cols-[minmax(0,1fr)_auto]"
          class:opacity-80={task.state === "queued"}
        >
          {#if task.state === "running"}
            <span class="absolute inset-y-0 left-0 w-1 animate-pulse bg-busy"></span>
          {/if}
          <div class="min-w-0 px-5 py-3.5">
            <div class="flex flex-wrap items-center gap-2">
              <span class="inline-flex items-center gap-1.5 text-[11px] uppercase tracking-wide {meta.text}">
                <span class="h-1.5 w-1.5 rounded-full {meta.dot} {task.state === 'running' ? 'animate-pulse' : ''}"></span>
                {meta.label}
              </span>
              <span class="rounded border border-line bg-surface-2 px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-wide text-faint">
                {kindLabel[task.kind] || task.kind}
              </span>
              {#if href}
                <a {href} use:link class="truncate font-mono text-[13px] font-semibold hover:text-busy">{displayId(task.id)}</a>
              {:else}
                <span class="truncate font-mono text-[13px] font-semibold">{displayId(task.id)}</span>
              {/if}
            </div>
            {#if task.state === "running" && steps(task.id).length > 0}
              <div class="mt-2 flex flex-wrap gap-1.5">
                {#each steps(task.id) as label}
                  <span class="rounded border border-busy/40 bg-surface-2 px-1.5 py-0.5 text-[11px] text-busy">{label}</span>
                {/each}
              </div>
            {:else if task.title}
              <div class="mt-1 text-[12px] text-faint">{task.title}</div>
            {/if}
          </div>
          <div class="flex items-center gap-2 border-t border-line px-5 py-2.5 text-[11px] text-faint sm:border-l sm:border-t-0">
            <span class="whitespace-nowrap">{timing(task)}</span>
            <div class="ml-auto flex items-center gap-1.5">
              {#if task.state === "queued"}
                <button
                  class="btn btn-sm inline-flex items-center gap-1 !py-1 !text-[11px]"
                  title="Move to the front of the queue"
                  disabled={busySeq === task.seq}
                  onclick={() => bump(task)}
                >
                  <Icon name="arrow-up" size={12} />
                  Bump
                </button>
                <button
                  class="btn btn-sm inline-flex items-center gap-1 !py-1 !text-[11px] text-err"
                  title="Remove from the queue"
                  disabled={busySeq === task.seq}
                  onclick={() => cancelQueued(task)}
                >
                  <Icon name="x" size={12} />
                  Cancel
                </button>
              {:else if task.state === "running" && canCancelRunning(task)}
                <button
                  class="btn btn-sm inline-flex items-center gap-1 !py-1 !text-[11px] text-err"
                  title="Abort the running job"
                  disabled={busySeq === task.seq}
                  onclick={() => cancelRunning(task)}
                >
                  <Icon name="x" size={12} />
                  Stop
                </button>
              {/if}
            </div>
          </div>
        </div>
      {/each}
    </div>

    {#if liveTotalPages > 1}
      <div class="mt-2 flex min-h-8 items-center justify-between px-1 text-[11px] text-faint">
        <span>
          {(currentLivePage - 1) * pageSize + 1}-{Math.min(currentLivePage * pageSize, live.length)} of
          {live.length}
        </span>
        <div class="flex items-center gap-1">
          <button
            class="btn inline-flex h-7 w-7 items-center justify-center !p-0"
            aria-label="Previous page"
            disabled={currentLivePage === 1}
            onclick={() => (livePage = currentLivePage - 1)}>&lt;</button
          >
          <span class="min-w-12 text-center">{currentLivePage} / {liveTotalPages}</span>
          <button
            class="btn inline-flex h-7 w-7 items-center justify-center !p-0"
            aria-label="Next page"
            disabled={currentLivePage === liveTotalPages}
            onclick={() => (livePage = currentLivePage + 1)}>&gt;</button
          >
        </div>
      </div>
    {/if}
  {/if}

  {#if recent.length > 0}
    <div class="mb-1 mt-5 text-[11px] font-medium uppercase tracking-wider text-faint">Recent</div>
    <div class="card divide-y divide-line overflow-hidden">
      {#each recent.slice(0, 8) as task (task.seq)}
        {@const meta = stateMeta[task.state] || stateMeta.done}
        <div class="flex items-center gap-3 px-4 py-2.5 text-[13px]">
          <span class="inline-flex w-20 shrink-0 items-center gap-1.5 text-[11px] uppercase tracking-wide {meta.text}">
            <span class="h-1.5 w-1.5 rounded-full {meta.dot}"></span>
            {meta.label}
          </span>
          <span class="w-16 shrink-0 font-mono text-[10px] uppercase tracking-wide text-faint">
            {kindLabel[task.kind] || task.kind}
          </span>
          <span class="min-w-0 flex-1 truncate font-mono text-[12px]">{displayId(task.id)}</span>
          {#if task.error}
            <span class="hidden max-w-[20rem] truncate text-[11px] text-err sm:inline" title={task.error}>{task.error}</span>
          {/if}
          <span class="shrink-0 text-[11px] text-faint">{timing(task)}</span>
        </div>
      {/each}
    </div>
  {/if}
{/if}

<div class="mt-6 border-t border-line pt-4">
    <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
      <div class="text-[11px] font-medium uppercase tracking-wider text-faint">Repository refresh pipeline</div>
    <div class="flex flex-wrap gap-1.5 text-[10px]">
      <span class="rounded border border-line px-1.5 py-0.5 text-faint">dependency path</span>
      <span class="rounded border border-accent/40 px-1.5 py-0.5 text-accent">independent branches</span>
    </div>
  </div>

  <div class="card overflow-x-auto p-4">
    <div class="flex min-w-[700px] items-center">
      <div class="w-28 flex-shrink-0 rounded border border-line bg-surface-2 px-3 py-2">
        <div class="font-mono text-[10px] uppercase tracking-wide text-faint">Git</div>
        <div class="mt-0.5 text-[12px] font-medium">Sync</div>
      </div>

      <div class="w-10 flex-shrink-0 border-t border-line"></div>

      <div class="w-28 flex-shrink-0 rounded border border-line bg-surface-2 px-3 py-2">
        <div class="font-mono text-[10px] uppercase tracking-wide text-faint">Graph</div>
        <div class="mt-0.5 text-[12px] font-medium">Build graph</div>
      </div>

      <div class="relative h-[94px] w-12 flex-shrink-0">
        <span class="absolute left-0 top-1/2 w-4 border-t border-line"></span>
        <span class="absolute bottom-[19px] left-4 top-[19px] border-l border-accent/60"></span>
        <span class="absolute left-4 top-[19px] w-8 border-t border-accent/60"></span>
        <span class="absolute bottom-[19px] left-4 w-8 border-t border-accent/60"></span>
      </div>

      <div class="flex flex-col gap-3">
        <div
          class="w-32 rounded border border-accent/40 bg-surface-2 px-3 py-2"
          title="Uses graph symbols for chunk boundaries; falls back to line windows when unavailable"
        >
          <div class="font-mono text-[10px] uppercase tracking-wide text-faint">Embeddings</div>
          <div class="mt-0.5 text-[12px] font-medium">Code index</div>
        </div>

        <div class="flex items-center">
          <div class="w-32 rounded border border-accent/40 bg-surface-2 px-3 py-2">
            <div class="font-mono text-[10px] uppercase tracking-wide text-faint">LLM</div>
            <div class="mt-0.5 text-[12px] font-medium">Docs</div>
          </div>
          <div class="w-10 flex-shrink-0 border-t border-line"></div>
          <div class="w-32 rounded border border-line bg-surface-2 px-3 py-2">
            <div class="font-mono text-[10px] uppercase tracking-wide text-faint">Embeddings</div>
            <div class="mt-0.5 text-[12px] font-medium">Docs index</div>
          </div>
        </div>
      </div>
    </div>

    <div class="mt-4 grid gap-2 border-t border-line pt-3 text-[11px] text-faint sm:grid-cols-2">
      <div>
        <span class="font-medium text-dim">New or changed repository:</span>
        Sync and Graph finish first. Code index then runs independently while Docs generates; Docs index waits only for
        Docs.
      </div>
      <div>
        <span class="font-medium text-dim">Unchanged repository:</span>
        Refresh stops after Sync and reuses existing artifacts. A settings reindex skips Git and Graph entirely.
      </div>
      <div>
        <span class="font-medium text-dim">Queue concurrency:</span>
        Caps top-level repository and source tasks. Extra work waits FIFO until a slot is free.
      </div>
      <div>
        <span class="font-medium text-dim">Docs concurrency:</span>
        Separately caps parallel LLM summary groups inside one Docs stage; configure both limits in Settings.
      </div>
    </div>
  </div>
</div>
