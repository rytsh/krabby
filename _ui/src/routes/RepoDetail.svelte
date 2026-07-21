<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { link } from "../lib/router.js";
  import Status from "../lib/Status.svelte";

  export let repoId;

  let repo = null;
  let error = "";
  let entries = [];
  let cwd = "";
  let selected = null;
  let fileContent = null;
  let fileError = "";

  // Browser mode: "files" (source tree) or "docs" (generated markdown).
  let mode = "files";
  let docList = [];
  let docsError = "";
  let selectedDoc = null;
  let docContent = null;

  async function loadDocs() {
    docsError = "";
    try {
      docList = await api.docs(repoId);
    } catch (e) {
      docsError = e.message;
      docList = [];
    }
  }

  async function openDoc(d) {
    selectedDoc = d.path;
    docContent = null;
    docsError = "";
    try {
      const res = await api.doc(repoId, d.path);
      docContent = res.content;
    } catch (e) {
      docsError = e.message;
    }
  }

  function setMode(m) {
    mode = m;
    if (m === "docs" && docList.length === 0 && !docsError) loadDocs();
  }

  async function loadRepo() {
    try {
      repo = await api.repo(repoId);
    } catch (e) {
      error = e.message;
    }
  }

  async function loadDir(dir) {
    fileError = "";
    try {
      entries = await api.files(repoId, dir, false);
      cwd = dir;
    } catch (e) {
      fileError = e.message;
      entries = [];
    }
  }

  async function open(entry) {
    if (entry.is_dir) {
      selected = null;
      fileContent = null;
      await loadDir(entry.path);
      return;
    }
    selected = entry.path;
    fileContent = null;
    fileError = "";
    try {
      fileContent = await api.file(repoId, entry.path);
    } catch (e) {
      fileError = e.message;
    }
  }

  function up() {
    if (!cwd) return;
    const parent = cwd.includes("/") ? cwd.slice(0, cwd.lastIndexOf("/")) : "";
    selected = null;
    fileContent = null;
    loadDir(parent);
  }

  async function refresh() {
    try {
      await api.refreshRepo(repoId);
      await loadRepo();
    } catch (e) {
      error = e.message;
    }
  }

  function fmt(ts) {
    if (!ts || ts.startsWith("0001")) return "—";
    return new Date(ts).toLocaleString();
  }

  function baseName(p) {
    return p.includes("/") ? p.slice(p.lastIndexOf("/") + 1) : p;
  }

  onMount(async () => {
    await loadRepo();
    await loadDir("");
  });
</script>

<div class="mb-4">
  <a href="/repos" use:link class="mb-2 inline-block text-[13px] text-dim hover:text-fg">← Repositories</a>
  <h1 class="font-mono text-xl font-semibold">{repoId}</h1>
</div>

{#if error}
  <div class="err-box">{error}</div>
{/if}

{#if repo}
  <div class="card mb-3 grid grid-cols-2 gap-x-6 gap-y-3 p-4">
    <div><span class="inline-block w-24 text-[13px] text-dim">Status</span><Status status={repo.status} /></div>
    <div>
      <span class="inline-block w-24 text-[13px] text-dim">Commit</span>
      <span class="font-mono text-[13px] text-faint">{repo.last_commit ? repo.last_commit.slice(0, 12) : "—"}</span>
    </div>
    <div>
      <span class="inline-block w-24 text-[13px] text-dim">Branch</span>
      <span class="text-[13px] text-faint">{repo.branch || "default"}</span>
    </div>
    <div>
      <span class="inline-block w-24 text-[13px] text-dim">Last build</span>
      <span class="text-[13px] text-faint">{fmt(repo.last_build_at)}</span>
    </div>
    <div>
      <span class="inline-block w-24 text-[13px] text-dim">Last sync</span>
      <span class="text-[13px] text-faint">{fmt(repo.last_sync_at)}</span>
    </div>
    <div class="col-span-2">
      <span class="inline-block w-24 text-[13px] text-dim">URL</span>
      <span class="font-mono text-[13px] text-faint">{repo.url}</span>
    </div>
    {#if repo.last_error}
      <div class="col-span-2">
        <span class="inline-block w-24 text-[13px] text-dim align-top">Error</span>
        <span class="text-[13px] text-err">{repo.last_error}</span>
      </div>
    {/if}
  </div>

  <div class="mb-5 flex gap-2">
    <a href={`/api/v1/repos/${repoId}/html`} target="_blank" rel="noreferrer"><button class="btn">Open graph HTML</button></a>
    <a href={`/api/v1/repos/${repoId}/report`} target="_blank" rel="noreferrer"><button class="btn">Report</button></a>
    <a href={`/api/v1/repos/${repoId}/graph`} target="_blank" rel="noreferrer"><button class="btn">graph.json</button></a>
    <button class="btn" on:click={refresh}>Refresh</button>
  </div>
{/if}

<div class="mb-3 flex gap-1">
  <button class="btn btn-sm" class:btn-primary={mode === "files"} on:click={() => setMode("files")}>Files</button>
  <button class="btn btn-sm" class:btn-primary={mode === "docs"} on:click={() => setMode("docs")}>Docs</button>
</div>

<div class="grid grid-cols-[280px_1fr] items-start gap-3">
  {#if mode === "files"}
    <div class="card max-h-[70vh] overflow-auto">
      <div class="sticky top-0 flex items-center gap-2 border-b border-line bg-surface px-3 py-2.5">
        <span class="text-[13px] text-dim">Files</span>
        <span class="font-mono text-[13px] text-faint">/{cwd}</span>
        {#if cwd}<button class="btn btn-sm ml-auto" on:click={up}>up</button>{/if}
      </div>
      {#if fileError}
        <div class="err-box m-2">{fileError}</div>
      {/if}
      <ul class="m-0 list-none p-1.5">
        {#each entries as e}
          <li>
            <button
              class="flex w-full items-center gap-1.5 rounded-md px-2 py-1.5 text-left text-[13px] text-dim hover:bg-surface-2 hover:text-fg"
              class:!bg-surface-2={selected === e.path}
              class:!text-fg={selected === e.path}
              on:click={() => open(e)}
            >
              <span class="w-3 text-faint">{e.is_dir ? "▸" : ""}</span>
              <span class="font-mono">{baseName(e.path)}</span>
            </button>
          </li>
        {/each}
        {#if entries.length === 0 && !fileError}
          <li class="p-4 text-dim">empty</li>
        {/if}
      </ul>
    </div>

    <div class="card max-h-[70vh] min-h-[200px] overflow-auto">
      {#if selected}
        <div class="sticky top-0 border-b border-line bg-surface px-3.5 py-2.5 font-mono text-xs text-faint">
          {selected}
          {#if fileContent && fileContent.truncated}<span class="ml-2 text-warn">truncated</span>{/if}
        </div>
        {#if fileContent}
          <pre class="m-0 overflow-x-auto p-3.5 font-mono text-[12.5px] leading-relaxed">{fileContent.content}</pre>
        {:else if !fileError}
          <div class="p-4 text-dim">Loading…</div>
        {/if}
      {:else}
        <div class="p-4 text-dim">Select a file to view its source.</div>
      {/if}
    </div>
  {:else}
    <div class="card max-h-[70vh] overflow-auto">
      <div class="sticky top-0 flex items-center gap-2 border-b border-line bg-surface px-3 py-2.5">
        <span class="text-[13px] text-dim">Generated docs</span>
        <span class="font-mono text-[13px] text-faint">{docList.length}</span>
      </div>
      {#if docsError}
        <div class="err-box m-2">{docsError}</div>
      {/if}
      <ul class="m-0 list-none p-1.5">
        {#each docList as d}
          <li>
            <button
              class="flex w-full flex-col gap-0.5 rounded-md px-2 py-1.5 text-left text-[13px] text-dim hover:bg-surface-2 hover:text-fg"
              class:!bg-surface-2={selectedDoc === d.path}
              class:!text-fg={selectedDoc === d.path}
              on:click={() => openDoc(d)}
            >
              <span class="font-mono">{d.title || d.path}</span>
              {#if d.source_path}<span class="text-[11px] text-faint">{d.source_path}</span>{/if}
            </button>
          </li>
        {/each}
        {#if docList.length === 0 && !docsError}
          <li class="p-4 text-dim">No generated docs. Enable doc generation in Settings, then refresh the repo.</li>
        {/if}
      </ul>
    </div>

    <div class="card max-h-[70vh] min-h-[200px] overflow-auto">
      {#if selectedDoc}
        <div class="sticky top-0 border-b border-line bg-surface px-3.5 py-2.5 font-mono text-xs text-faint">
          {selectedDoc}
        </div>
        {#if docContent !== null}
          <pre class="m-0 overflow-x-auto whitespace-pre-wrap p-3.5 text-[13px] leading-relaxed">{docContent}</pre>
        {:else if !docsError}
          <div class="p-4 text-dim">Loading…</div>
        {/if}
      {:else}
        <div class="p-4 text-dim">Select a document to view it.</div>
      {/if}
    </div>
  {/if}
</div>
