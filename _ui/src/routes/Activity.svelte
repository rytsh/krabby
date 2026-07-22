<script>
  import { onDestroy, onMount } from "svelte";
  import { loadRepos, repos, reposLoaded } from "../lib/repos.js";
  import { link } from "../lib/router.js";
  import Icon from "../lib/Icon.svelte";

  const jobMeta = {
    sync: {
      label: "Syncing repository",
      detail: "Fetching remote changes and updating the local clone.",
      phase: "Git",
    },
    graph: {
      label: "Building knowledge graph",
      detail: "Extracting symbols, relationships and graph artifacts.",
      phase: "Graph",
    },
    docs: {
      label: "Generating documentation",
      detail: "Summarizing source files and synthesizing documentation with the LLM.",
      phase: "LLM",
    },
    docs_index: {
      label: "Indexing documentation",
      detail: "Chunking generated docs and creating embeddings for semantic search.",
      phase: "Embeddings",
    },
    code_index: {
      label: "Indexing source code",
      detail: "Chunking source files and creating code-search embeddings.",
      phase: "Embeddings",
    },
  };

  function describe(repo, step) {
    const meta = jobMeta[step] || {
      label: step || "Background work",
      detail: "Krabby is processing this repository.",
      phase: "Worker",
    };
    return { repo: repo.id, step, ...meta };
  }

  // repo.running is comma-joined when several steps run in parallel
  // (e.g. "code_index,docs"); show one job entry per step.
  let jobs = $derived(
    $repos
      .filter((repo) => repo.running)
      .flatMap((repo) => repo.running.split(",").map((step) => describe(repo, step)))
      .sort((a, b) => a.repo.localeCompare(b.repo) || a.step.localeCompare(b.step)),
  );
  const jobsPageSize = 6;
  let jobsPage = $state(1);
  let jobsTotalPages = $derived(Math.max(1, Math.ceil(jobs.length / jobsPageSize)));
  let currentJobsPage = $derived(Math.min(jobsPage, jobsTotalPages));
  let visibleJobs = $derived(
    jobs.slice((currentJobsPage - 1) * jobsPageSize, currentJobsPage * jobsPageSize),
  );

  const LIVE_INTERVAL_KEY = "krabby-activity-live-interval";
  const liveIntervals = [0, 4_000, 10_000, 30_000, 60_000];
  const savedLiveInterval = Number(localStorage.getItem(LIVE_INTERVAL_KEY));
  let liveInterval = $state(liveIntervals.includes(savedLiveInterval) ? savedLiveInterval : 0);
  let refreshing = $state(false);
  let timer;

  async function refresh() {
    refreshing = true;
    try {
      await loadRepos();
    } finally {
      refreshing = false;
    }
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
      Live repository work running on the server. Jobs continue when you navigate away from their repository page.
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

<div class="mb-4 grid grid-cols-2 gap-3 sm:max-w-md">
  <div class="card px-4 py-3">
    <div class="font-mono text-2xl font-semibold leading-none">{jobs.length}</div>
    <div class="mt-1.5 text-[12px] text-faint">Active jobs</div>
  </div>
  <div class="card px-4 py-3">
    <div class="font-mono text-2xl font-semibold leading-none">{$repos.length}</div>
    <div class="mt-1.5 text-[12px] text-faint">Tracked repositories</div>
  </div>
</div>

{#if !$reposLoaded}
  <div class="card p-8 text-center text-dim">Loading activity…</div>
{:else if jobs.length === 0}
  <div class="card flex min-h-44 flex-col items-center justify-center border-dashed px-6 text-center">
    <div class="font-medium">No background jobs are running</div>
    <div class="mt-1 text-[13px] text-faint">Refresh a repository or generate an artifact to see it here.</div>
  </div>
{:else}
  <div class="flex flex-col gap-3">
    {#each visibleJobs as job (job.repo)}
      <a
        href={`/repos/${job.repo}`}
        use:link
        class="card group relative grid overflow-hidden transition-colors hover:border-busy sm:grid-cols-[minmax(0,1fr)_auto]"
      >
        <span class="absolute inset-y-0 left-0 w-1 animate-pulse bg-busy"></span>
        <div class="min-w-0 px-5 py-4">
          <div class="flex flex-wrap items-center gap-2">
            <span class="truncate font-mono text-[13px] font-semibold">{job.repo}</span>
            <span class="rounded border border-line bg-surface-2 px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-wide text-faint">
              {job.phase}
            </span>
          </div>
          <div class="mt-2 text-[14px] font-medium">{job.label}</div>
          <div class="mt-0.5 text-[12px] text-faint">{job.detail}</div>
        </div>
        <div class="flex items-center gap-2 border-t border-line px-5 py-3 text-[11px] uppercase tracking-wider text-busy sm:border-l sm:border-t-0">
          Running
        </div>
      </a>
    {/each}

    {#if jobsTotalPages > 1}
      <div class="flex min-h-8 items-center justify-between px-1 text-[11px] text-faint">
        <span>
          {(currentJobsPage - 1) * jobsPageSize + 1}-{Math.min(currentJobsPage * jobsPageSize, jobs.length)} of
          {jobs.length} jobs
        </span>
        <div class="flex items-center gap-1">
          <button
            class="btn inline-flex h-7 w-7 items-center justify-center !p-0"
            aria-label="Previous jobs page"
            disabled={currentJobsPage === 1}
            onclick={() => (jobsPage = currentJobsPage - 1)}
          >&lt;</button>
          <span class="min-w-12 text-center">{currentJobsPage} / {jobsTotalPages}</span>
          <button
            class="btn inline-flex h-7 w-7 items-center justify-center !p-0"
            aria-label="Next jobs page"
            disabled={currentJobsPage === jobsTotalPages}
            onclick={() => (jobsPage = currentJobsPage + 1)}
          >&gt;</button>
        </div>
      </div>
    {/if}
  </div>
{/if}

<div class="mt-6 border-t border-line pt-4">
  <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
    <div class="text-[11px] font-medium uppercase tracking-wider text-faint">Tracked work types</div>
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

    <div class="mt-4 flex flex-wrap gap-x-5 gap-y-1 border-t border-line pt-3 text-[11px] text-faint">
      <span>Within one repository: scheduled sequentially</span>
      <span>Across repositories: can run in parallel</span>
      <span>Code index uses Graph symbols for chunking, with a line-window fallback</span>
      <span>Code index and Docs can branch independently after Graph</span>
    </div>
  </div>
</div>
