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

  function describe(repo) {
    const meta = jobMeta[repo.running] || {
      label: repo.running || "Background work",
      detail: "Krabby is processing this repository.",
      phase: "Worker",
    };
    return { repo: repo.id, step: repo.running, ...meta };
  }

  let jobs = $derived($repos.filter((repo) => repo.running).map(describe));

  let live = $state(false);
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

  function setLive(enabled) {
    live = enabled;
    clearInterval(timer);
    timer = undefined;
    if (live) {
      refresh();
      timer = setInterval(refresh, 4000);
    }
  }

  onMount(refresh);
  onDestroy(() => clearInterval(timer));
</script>

<div class="mb-5 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
  <div>
    <p class="m-0 max-w-2xl text-dim">
      Live repository work running on the server. Jobs continue when you navigate away from their repository page.
    </p>
  </div>
  <div class="flex items-center gap-2">
    <label class="btn btn-sm inline-flex items-center gap-2">
      <input type="checkbox" checked={live} onchange={(e) => setLive(e.currentTarget.checked)} />
      Live
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
    {#each jobs as job (job.repo)}
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
  </div>
{/if}

<div class="mt-6 border-t border-line pt-4">
  <div class="mb-2 text-[11px] font-medium uppercase tracking-wider text-faint">Tracked work types</div>
  <div class="flex flex-wrap gap-2">
    {#each Object.values(jobMeta) as meta (meta.label)}
      <span class="rounded-md border border-line bg-surface px-2.5 py-1 text-[11px] text-dim">{meta.label}</span>
    {/each}
  </div>
</div>
