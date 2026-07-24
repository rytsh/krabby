<script>
  import { onMount, onDestroy } from "svelte";
  import { api } from "../lib/api.js";
  import { path as routePath, link } from "../lib/router.js";
  import { fmtDate } from "../lib/format.js";
  import Status from "../lib/Status.svelte";
  import CodeView from "../lib/CodeView.svelte";
  import FileTree from "../lib/FileTree.svelte";
  import MarkdownView from "../lib/MarkdownView.svelte";
  import Icon from "../lib/Icon.svelte";
  import { successToast } from "../lib/toast.js";

  let { repoId } = $props();

  const POLL_INTERVAL_KEY = "krabby-repo-poll-interval";
  const pollIntervals = [0, 3_000, 5_000, 10_000, 30_000];
  const savedPollInterval = localStorage.getItem(POLL_INTERVAL_KEY);
  const initialPollInterval = Number(savedPollInterval);
  let pollInterval = $state(
    savedPollInterval !== null && pollIntervals.includes(initialPollInterval) ? initialPollInterval : 5_000,
  );

  let repo = $state(null);
  let error = $state("");

  // Generation stages shown in the Artifacts card. Enabled flags come from the
  // docs config; graph generation is always available. `needs` mirrors the
  // backend stageDeps so the UI can tell the user which prerequisites a stage
  // will build automatically when their output is missing (see manager.go).
  const stageDefs = [
    { key: "graph", label: "Graph", needs: [] },
    { key: "docs", label: "Docs", needs: ["graph"] },
    { key: "docs_index", label: "Docs index", needs: ["docs"] },
    { key: "code_index", label: "Code index", needs: ["graph"] },
  ];
  let cfg = $state(null);
  let generating = $state({});
  let forcing = $state({});

  // Stages that support a forced rebuild: docs regeneration is incremental
  // (unchanged sources reuse cached summaries and documentation.md), so a plain
  // Generate on an up-to-date repo is a no-op. Force bypasses those caches.
  const forceable = new Set(["docs", "docs_index"]);

  function stageEnabled(key) {
    if (!cfg) return true;
    if (key === "graph") return true;
    if (key === "docs") return cfg.docs_enabled;
    if (key === "docs_index") return cfg.rag_enabled;
    if (key === "code_index") return true;
    return true;
  }


  async function generate(key, force = false) {
    if (force) forcing = { ...forcing, [key]: true };
    else generating = { ...generating, [key]: true };
    try {
      await api.generate(repoId, [key], force);
      await loadRepo();
    } catch (e) {
      error = e.message;
    } finally {
      if (force) forcing = { ...forcing, [key]: false };
      else generating = { ...generating, [key]: false };
    }
  }

  let selected = $state(null);
  let fileContent = $state(null);
  let fileError = $state("");

  // File tree state: root entries plus lazily-loaded children per directory.
  let rootEntries = $state([]);
  let rootLoaded = false;
  let fileSnapshot = $state("");
  let expanded = $state({});
  let children = $state({});
  let fileBrowserVersion = 0;

  // Browser mode: "docs" (default: comprehensive markdown) or "files".
  let mode = $state("docs");
  let docList = $state([]);
  let docsError = $state("");
  let selectedDoc = $state(null);
  let docContent = $state(null);
  // Doc display: rendered HTML or raw markdown.
  let docView = $state("html");
  let docHeadings = $state([]);
  let docBrowserOpen = $state(false);
  let docsVersion;

  function jumpToHeading(id) {
    document.getElementById(id)?.scrollIntoView({ behavior: "smooth", block: "start" });
  }

  // Resizable split: left pane width in px, persisted.
  let paneW = $state(Number(localStorage.getItem("krabby-pane-w")) || 260);

  function startDrag(e) {
    e.preventDefault();
    const startX = e.clientX;
    const startW = paneW;

    function move(ev) {
      paneW = Math.min(640, Math.max(160, startW + ev.clientX - startX));
    }

    function up() {
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      localStorage.setItem("krabby-pane-w", String(paneW));
    }

    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
  }

  async function loadDocs() {
    docsError = "";
    try {
      const docs = await api.docs(repoId);
      docList = Array.isArray(docs) ? docs : [];
      // Single comprehensive document: open it right away.
      if (docList.length > 0 && !selectedDoc) openDoc(docList[0]);
    } catch (e) {
      docsError = e.message;
      docList = [];
    }
  }

  async function openDoc(d) {
    selectedDoc = d.path;
    docContent = null;
    docHeadings = [];
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
    if (m === "files" && !rootLoaded) loadRoot();
  }

  async function loadRepo() {
    try {
      const next = await api.repo(repoId);
      const snapshotChanged = repo?.path && next?.path && repo.path !== next.path;
      const docsStage = next?.stages?.docs;
      const nextDocsVersion = docsStage?.finished_at || "";
      const docsFinished =
        docsVersion !== undefined && docsStage?.status === "ok" && nextDocsVersion !== docsVersion;

      docsVersion = nextDocsVersion;
      repo = next;
      if (snapshotChanged) {
        fileBrowserVersion += 1;
        fileSnapshot = "";
        rootLoaded = false;
        rootEntries = [];
        expanded = {};
        children = {};
        selected = null;
        fileContent = null;
        if (mode === "files") await loadRoot();
      }
      if (docsFinished) await loadDocs();
    } catch (e) {
      error = e.message;
    }
  }

  function sortEntries(list) {
    // Directories first, then files, both alphabetical.
    return [...list].sort((a, b) => b.is_dir - a.is_dir || a.path.localeCompare(b.path));
  }

  async function loadRoot() {
    const version = fileBrowserVersion;
    fileError = "";
    rootLoaded = true;
    try {
      const result = await api.files(repoId, "", false);
      if (version !== fileBrowserVersion) return;
      fileSnapshot = result.snapshot;
      expanded = {};
      children = {};
      rootEntries = sortEntries(result.entries);
    } catch (e) {
      fileError = e.message;
      rootEntries = [];
    }
  }

  // Expand/collapse a directory in place; fetch its children on first expand.
  async function toggleDir(entry) {
    const version = fileBrowserVersion;
    const p = entry.path;
    if (expanded[p]) {
      expanded = { ...expanded, [p]: false };
      return;
    }
    expanded = { ...expanded, [p]: true };
    if (!children[p]) {
      try {
        const result = await api.files(repoId, p, false, fileSnapshot);
        if (version !== fileBrowserVersion) return;
        children = { ...children, [p]: sortEntries(result.entries) };
      } catch (e) {
        fileError = e.message;
        expanded = { ...expanded, [p]: false };
      }
    }
  }

  async function openFile(entry) {
    const version = fileBrowserVersion;
    selected = entry.path;
    fileContent = null;
    fileError = "";
    try {
      const content = await api.file(repoId, entry.path, fileSnapshot);
      if (version !== fileBrowserVersion) return;
      fileContent = content;
    } catch (e) {
      fileError = e.message;
    }
  }

  // Line to scroll to in the viewer (deep links from code search).
  let targetLine = $state(0);

  function openFromTree(entry) {
    targetLine = 0;
    openFile(entry);
  }

  // Expand every ancestor directory of a file so it is visible in the tree.
  async function revealFile(file) {
    const version = fileBrowserVersion;
    if (!rootLoaded) await loadRoot();
    const parts = file.split("/");
    let prefix = "";
    for (let i = 0; i < parts.length - 1; i++) {
      prefix = prefix ? `${prefix}/${parts[i]}` : parts[i];
      if (!children[prefix]) {
        try {
          const result = await api.files(repoId, prefix, false, fileSnapshot);
          if (version !== fileBrowserVersion) return;
          children = { ...children, [prefix]: sortEntries(result.entries) };
        } catch {
          break;
        }
      }
      expanded = { ...expanded, [prefix]: true };
    }
  }

  // Deep links from search open either a source file at a line or a generated
  // document. The last-link keys are intentionally untracked so writes do not
  // re-trigger the effect.
  let lastLink = "";
  let lastDocLink = "";

  async function openFromLink(file, line) {
    mode = "files";
    targetLine = line;
    await revealFile(file);
    await openFile({ path: file, is_dir: false });
  }

  async function refresh() {
    try {
      await api.refreshRepo(repoId);
      successToast("Refresh queued");
      await loadRepo();
    } catch (e) {
      error = e.message;
    }
  }

  async function cancelJob() {
    try {
      await api.cancelRepoJob(repoId);
      successToast("Cancel requested");
      await loadRepo();
    } catch (e) {
      error = e.message;
    }
  }

  let timer;
  function startPolling() {
    clearInterval(timer);
    timer = pollInterval > 0 ? setInterval(loadRepo, pollInterval) : undefined;
  }

  function setPollInterval(value) {
    pollInterval = Number(value);
    localStorage.setItem(POLL_INTERVAL_KEY, String(pollInterval));
    startPolling();
  }

  onMount(async () => {
    await loadRepo();
    api.docsConfig().then((c) => (cfg = c)).catch(() => {});
    // Poll so stage states and the running indicator update live.
    startPolling();
    await loadDocs();
  });
  onDestroy(() => clearInterval(timer));
  // Derived rows so the card re-renders when the polled repo record changes.
  let stageRows = $derived(stageDefs.map((s) => {
    const st = (repo && repo.stages && repo.stages[s.key]) || {};
    // repo.running is the live in-memory worker state (comma-joined when several
    // steps run in parallel). A persisted "running" stage can be left behind by
    // an interrupted process and is not active.
    const running = (repo?.running || "").split(",").includes(s.key);
    return {
      ...s,
      st,
      running,
      enabled: cfg ? stageEnabled(s.key) : true,
      stale:
        st.status === "ok" && st.commit && repo && repo.last_commit && st.commit !== repo.last_commit,
    };
  }));
  let hasRunningStage = $derived(stageRows.some((s) => s.running));
  $effect(() => {
    const params = new URLSearchParams($routePath.split("?")[1] || "");
    const f = params.get("file");
    const doc = params.get("doc");
    const ln = Number(params.get("line")) || 0;
    if (doc && doc !== lastDocLink) {
      lastDocLink = doc;
      mode = "docs";
      openDoc({ path: doc });
      return;
    }
    const key = f ? `${f}#${ln}` : "";
    if (f && key !== lastLink) {
      lastLink = key;
      openFromLink(f, ln);
    }
  });
</script>

<div class="grid grid-cols-[minmax(0,1fr)_260px] items-start gap-4">
  <div class="min-w-0">
    {#if mode === "files"}
      <div class="card flex h-[calc(100vh-72px)] overflow-hidden">
        <div class="flex h-full flex-shrink-0 flex-col overflow-auto" style={`width:${paneW}px`}>
          <div class="sticky top-0 z-10 flex items-center gap-2 border-b border-line bg-surface px-3 py-2.5">
            <span class="text-[13px] text-dim">Files</span>
          </div>
          <div class="p-1.5">
            <FileTree
              entries={rootEntries}
              {selected}
              {expanded}
              {children}
              onToggle={toggleDir}
              onOpen={openFromTree}
            />
            {#if rootEntries.length === 0 && !fileError}
              <div class="p-4 text-dim">empty</div>
            {/if}
          </div>
        </div>

        <div
          class="h-full w-[3px] flex-shrink-0 cursor-col-resize bg-line transition-colors hover:bg-accent"
          role="separator"
          aria-orientation="vertical"
          onpointerdown={startDrag}
        ></div>

        <div class="flex h-full min-w-0 flex-1 flex-col">
          {#if selected}
            <div class="flex-shrink-0 border-b border-line bg-surface px-3.5 py-2.5 font-mono text-xs text-faint">
              {selected}
              {#if fileContent && fileContent.truncated}<span class="ml-2 text-warn">truncated</span>{/if}
            </div>
            <div class="min-h-0 flex-1 overflow-auto" style={fileContent ? "background:#24292e" : ""}>
              {#if fileContent}
                <CodeView code={fileContent.content} path={selected} scrollTo={targetLine} />
              {:else if !fileError}
                <div class="p-4 text-dim">Loading…</div>
              {/if}
            </div>
          {:else}
            <div class="p-4 text-dim">Select a file to view its source.</div>
          {/if}
        </div>
      </div>
    {:else}
      <div class="card flex h-[calc(100vh-72px)] overflow-hidden">
        {#if docBrowserOpen}
          <div class="flex h-full flex-shrink-0 flex-col overflow-auto" style={`width:${paneW}px`}>
            <div class="sticky top-0 z-10 flex items-center gap-2 border-b border-line bg-surface px-3 py-2.5">
              <span class="text-[13px] text-dim">Docs</span>
              <span class="font-mono text-[13px] text-faint">{docList.length}</span>
            </div>
            <ul class="m-0 list-none p-1.5">
              {#each docList as d}
                <li>
                  <button
                    class="flex w-full flex-col gap-0.5 rounded-md px-2 py-1.5 text-left text-[13px] text-dim hover:bg-surface-2 hover:text-fg"
                    class:!bg-surface-2={selectedDoc === d.path}
                    class:!text-fg={selectedDoc === d.path}
                    onclick={() => openDoc(d)}
                  >
                    <span class="font-mono">{d.title || d.path}</span>
                    {#if d.source_path}<span class="text-[11px] text-faint">{d.source_path}</span>{/if}
                  </button>
                </li>
              {/each}
              {#if docList.length === 0 && !docsError}
                <li class="p-4 text-dim">
                  No documentation yet. Enable docs in Settings, then hit Generate on the Docs artifact.
                </li>
              {/if}
            </ul>
          </div>

          <div
            class="h-full w-[3px] flex-shrink-0 cursor-col-resize bg-line transition-colors hover:bg-accent"
            role="separator"
            aria-orientation="vertical"
            onpointerdown={startDrag}
          ></div>
        {/if}

        <div class="flex h-full min-w-0 flex-1 flex-col">
          <div class="z-10 flex flex-shrink-0 items-center gap-2 border-b border-line bg-surface px-3.5 py-2 font-mono text-xs text-faint">
            <button
              class="inline-flex h-6 w-6 flex-shrink-0 cursor-pointer items-center justify-center rounded text-faint transition-colors hover:bg-surface-2 hover:text-fg"
              title={docBrowserOpen ? "Hide document list" : "Show document list"}
              aria-label={docBrowserOpen ? "Hide document list" : "Show document list"}
              aria-expanded={docBrowserOpen}
              onclick={() => (docBrowserOpen = !docBrowserOpen)}
            >
              <Icon name={docBrowserOpen ? "panel-left-close" : "panel-left-open"} size={15} />
            </button>
            {#if selectedDoc}
              <span class="truncate">{selectedDoc}</span>
              <span class="ml-auto flex gap-1">
                <button class="view-toggle" class:view-toggle-active={docView === "html"} onclick={() => (docView = "html")}>
                  Rendered
                </button>
                <button class="view-toggle" class:view-toggle-active={docView === "md"} onclick={() => (docView = "md")}>
                  Markdown
                </button>
              </span>
            {/if}
          </div>

          <div class="flex min-h-0 min-w-0 flex-1 overflow-y-auto">
            <div class="min-w-0 flex-1">
              {#if selectedDoc}
                {#if docContent !== null}
                  {#if docView === "html"}
                    <MarkdownView markdown={docContent} onHeadings={(headings) => (docHeadings = headings)} />
                  {:else}
                    <pre class="m-0 overflow-x-auto whitespace-pre-wrap p-3.5 font-mono text-[12.5px] leading-relaxed">{docContent}</pre>
                  {/if}
                {:else if !docsError}
                  <div class="p-4 text-dim">Loading…</div>
                {/if}
              {:else}
                <div class="p-4 text-dim">Select a document to view it.</div>
              {/if}
            </div>

            {#if docView === "html" && docHeadings.length > 0}
              <aside class="sticky top-0 flex h-fit w-[220px] flex-shrink-0 flex-col self-start border-l border-line bg-surface/40 text-[12px]">
                <div class="flex items-center border-b border-line px-3 py-2.5">
                  <span class="font-medium">On this page</span>
                  <span class="ml-auto font-mono text-[11px] text-faint">{docHeadings.length}</span>
                </div>
                <nav class="py-1.5">
                  {#each docHeadings as heading (heading.id)}
                    <button
                      class="block w-full cursor-pointer truncate border-l-2 border-transparent py-1.5 pr-3 text-left text-dim transition-colors hover:border-accent hover:bg-surface-2 hover:text-fg"
                      style={`padding-left:${12 + (heading.level - 1) * 10}px`}
                      title={heading.text}
                      onclick={() => jumpToHeading(heading.id)}
                    >
                      {heading.text}
                    </button>
                  {/each}
                </nav>
              </aside>
            {/if}
          </div>
        </div>
      </div>
    {/if}
  </div>

  <div class="sticky flex max-h-[calc(100vh-72px)] w-[260px] flex-col gap-3 overflow-y-auto pr-1">
    <div class="card shrink-0 grid grid-cols-2 gap-1 p-1">
      <button
        class="flex cursor-pointer items-center justify-center gap-1.5 rounded-md px-2 py-1.5 text-[12px] text-faint transition-colors hover:bg-surface-2 hover:text-fg"
        class:!bg-surface-2={mode === "docs"}
        class:!text-fg={mode === "docs"}
        onclick={() => setMode("docs")}
      >
        <Icon name="book" size={14} />
        Docs
      </button>
      <button
        class="flex cursor-pointer items-center justify-center gap-1.5 rounded-md px-2 py-1.5 text-[12px] text-faint transition-colors hover:bg-surface-2 hover:text-fg"
        class:!bg-surface-2={mode === "files"}
        class:!text-fg={mode === "files"}
        onclick={() => setMode("files")}
      >
        <Icon name="file-code" size={14} />
        Files
      </button>
    </div>

    {#if repo}
      <div class="card shrink-0 overflow-hidden text-[12px]">
        <div class="flex items-center border-b border-line px-3 py-2">
          <span class="font-medium">Outputs</span>
          <label class="ml-auto inline-flex items-center gap-1.5 text-dim">
            Auto
            <select
              class="input !h-7 !w-auto !py-0 text-[12px]"
              value={pollInterval}
              onchange={(e) => setPollInterval(e.currentTarget.value)}
              aria-label="Repository status refresh interval"
            >
              <option value={0}>Off</option>
              <option value={3000}>3s</option>
              <option value={5000}>5s</option>
              <option value={10000}>10s</option>
              <option value={30000}>30s</option>
            </select>
          </label>
        </div>
        <div class="grid grid-cols-3 divide-x divide-line">
          <a
            href={`api/v1/repos/${repoId}/-/html`}
            target="_blank"
            rel="noreferrer"
            class="flex flex-col items-center gap-1.5 px-2 py-2.5 text-dim transition-colors hover:bg-surface-2 hover:text-fg"
          >
            <Icon name="graph" size={15} />
            Visualize
          </a>
          <a
            href={`api/v1/repos/${repoId}/-/report`}
            target="_blank"
            rel="noreferrer"
            class="flex flex-col items-center gap-1.5 px-2 py-2.5 text-dim transition-colors hover:bg-surface-2 hover:text-fg"
          >
            <Icon name="file-text" size={15} />
            Report
          </a>
          <a
            href={`api/v1/repos/${repoId}/-/graph`}
            target="_blank"
            rel="noreferrer"
            class="flex flex-col items-center gap-1.5 px-2 py-2.5 text-dim transition-colors hover:bg-surface-2 hover:text-fg"
          >
            <Icon name="braces" size={15} />
            JSON
          </a>
        </div>
      </div>

      <div class="card shrink-0 p-3 text-[13px]">
        <div class="mb-2 flex items-center justify-between gap-2">
          <span class="font-medium">Artifacts</span>
          {#if repo.running && !hasRunningStage}
            <button
              class="btn btn-sm btn-danger ml-auto !px-2 !py-0.5 text-[12px]"
              title="Abort the running job"
              onclick={cancelJob}
            >
              Cancel
            </button>
          {:else}
            <button
              class="btn btn-sm ml-auto inline-flex items-center gap-1.5 !px-2 !py-0.5 text-[12px]"
              disabled={repo.running}
              onclick={refresh}
              title="Pull remote changes and rebuild the repository"
            >
              <Icon name="refresh" size={13} />
              Refresh
            </button>
          {/if}
        </div>

        <div class="mb-2 rounded border border-line bg-surface-2 px-2.5 py-2 text-[11px] leading-relaxed text-faint">
          <span class="font-medium text-dim">Refresh pipeline:</span>
          Sync -> Graph, then <span class="text-accent">Code index || Docs</span>; Docs -> Docs index.
          <span class="block">Docs index waits for Docs only. Unchanged repos stop after Sync.</span>
        </div>

        <div class="flex flex-col">
          {#each stageRows as s (s.key)}
            <div class="border-t border-line py-2 first:border-t-0">
              <div class="flex items-center gap-2">
                <span
                  class="inline-block h-[7px] w-[7px] flex-shrink-0 rounded-[1px]"
                  class:animate-pulse={s.running}
                  style="background: {s.running
                    ? 'var(--color-busy)'
                    : s.st.status === 'ok'
                      ? 'var(--color-ok)'
                      : s.st.status === 'error'
                        ? 'var(--color-err)'
                        : 'var(--color-faint)'}"
                ></span>
                <span>{s.label}</span>
                {#if s.running}
                  <span class="text-[11px] text-busy">working</span>
                {:else if !s.enabled}
                  <span class="text-[11px] text-faint">disabled</span>
                {:else if s.stale}
                  <span
                    class="rounded border border-warn/40 px-1 text-[11px] text-warn"
                    title={`generated for ${s.st.commit.slice(0, 8)}, repo is at ${repo.last_commit.slice(0, 8)}`}
                  >stale</span>
                {/if}
                {#if s.running}
                  <button
                    class="btn btn-sm btn-danger ml-auto !px-2 !py-0.5 text-[12px]"
                    title="Abort the running job"
                    onclick={cancelJob}
                  >
                    Cancel
                  </button>
                {:else}
                  <div class="ml-auto flex gap-1">
                    {#if forceable.has(s.key)}
                      <button
                        class="btn btn-sm !px-2 !py-0.5 text-[12px]"
                        disabled={!s.enabled || generating[s.key] || forcing[s.key]}
                        onclick={() => generate(s.key, true)}
                        title={`Force-rebuild ${s.label}, ignoring the incremental cache (regenerates everything even if nothing changed)`}
                      >
                        {forcing[s.key] ? "Starting…" : "Force"}
                      </button>
                    {/if}
                    <button
                      class="btn btn-sm !px-2 !py-0.5 text-[12px]"
                      disabled={!s.enabled || generating[s.key] || forcing[s.key]}
                      onclick={() => generate(s.key)}
                      title={s.needs.length
                        ? `Rebuild ${s.label}; missing prerequisites (${s.needs.join(", ")}) are built automatically`
                        : `Rebuild ${s.label}`}
                    >
                      {generating[s.key] ? "Starting…" : "Generate"}
                    </button>
                  </div>
                {/if}
              </div>
              <div class="mt-0.5 pl-[15px] text-[11px] text-faint">
                {#if s.running}
                  working…
                {:else if s.st.status === "ok"}
                  {fmtDate(s.st.finished_at)}
                {:else if s.st.status === "error"}
                  <span class="text-err" title={s.st.error}>{s.st.error}</span>
                {:else}
                  not generated
                {/if}
              </div>
            </div>
          {/each}
        </div>
      </div>

      <div class="card shrink-0 flex flex-col gap-2.5 p-4 text-[13px]">
        <div class="flex items-center justify-between gap-2">
          <span class="text-dim">Status</span>
          <Status status={repo.status} />
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="text-dim">Commit</span>
          <span class="font-mono text-faint">{repo.last_commit ? repo.last_commit.slice(0, 12) : "—"}</span>
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="text-dim">Branch</span>
          <span class="text-faint">{repo.branch || "default"}</span>
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="text-dim">Namespace</span>
          <a
            href="/namespaces"
            use:link
            class="inline-flex items-center gap-1 font-mono text-faint transition-colors hover:text-fg"
            title="Namespace this repository belongs to"
          >
            <Icon name="tag" size={12} />
            {repo.namespace || "default"}
          </a>
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="text-dim">Last build</span>
          <span class="text-right text-faint">{fmtDate(repo.last_build_at)}</span>
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="text-dim">Last sync</span>
          <span class="text-right text-faint">{fmtDate(repo.last_sync_at)}</span>
        </div>
        <div class="flex flex-col gap-1">
          <span class="text-dim">URL</span>
          <span class="break-all font-mono text-faint">{repo.url}</span>
        </div>
        {#if repo.last_error}
          <div class="flex flex-col gap-1">
            <span class="text-dim">Error</span>
            <span class="break-words text-err">{repo.last_error}</span>
          </div>
        {/if}
      </div>
    {/if}
  </div>
</div>
