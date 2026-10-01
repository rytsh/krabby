<script>
  import { onDestroy, tick } from "svelte";
  import { push } from "svelte-spa-router";
  import { path, link } from "../lib/router.js";
  import { api } from "../lib/api.js";
  import { createLatestRequest } from "../lib/async.js";
  import { pictureDocumentTree, pictureDocumentLink, pictureURL } from "../lib/big-picture.js";
  import { successToast } from "../lib/toast.js";
  import { fmtDate } from "../lib/format.js";
  import FileTree from "../lib/FileTree.svelte";
  import MarkdownView from "../lib/MarkdownView.svelte";
  import BigPictureForm from "../components/BigPictureForm.svelte";

  let { name } = $props();
  let workspace = $state(null);
  let snapshot = $state(null);
  let document = $state(null);
  let selected = $state("");
  let expanded = $state({});
  let loading = $state(true);
  let busy = $state(false);
  let error = $state("");
  let tab = $state("documents");
  let publishing = $state(false);
  let publication = $state("");
  let publicationError = $state("");
  let alive = true;
  const requests = createLatestRequest();
  let params = $derived(new URLSearchParams($path.split("?")[1] || ""));
  let tree = $derived(pictureDocumentTree(snapshot?.documents || []));
  const runStatusLabels = {
    published: "published a new revision",
    unchanged: "collected evidence unchanged, model skipped",
    no_changes: "no document changes needed",
    failed: "failed; existing publication kept",
  };

  async function load(revision, docPath) {
    const request = requests.nextAbortable();
    loading = true;
    error = "";
    workspace = null;
    snapshot = null;
    document = null;
    try {
      const picture = await api.bigPicture(name, request.signal);
      if (!request.isLatest()) return;
      workspace = picture;
      if (!picture.current_revision && !revision) return;
      const manifest = await api.bigPictureSnapshot(name, revision, request.signal);
      if (!request.isLatest()) return;
      snapshot = manifest;
      const target = docPath || manifest.overview;
      if (!manifest.documents.some((doc) => doc.path === target)) throw new Error("Document not found in this publication.");
      selected = target;
      const segments = target.split("/");
      for (let i = 1; i < segments.length; i++) expanded[segments.slice(0, i).join("/")] = true;
      let offset = 0;
      let text = "";
      let part;
      do {
        part = await api.bigPictureDocument(name, manifest.id, target, offset, request.signal);
        if (!request.isLatest()) return;
        text += part.content;
        offset += part.bytes;
        if (part.truncated && part.bytes <= 0) throw new Error("Document pagination made no progress.");
      } while (part.truncated);
      document = { ...part, content: text };
    } catch (e) { if (request.isLatest() && e.name !== "AbortError") error = e.message; }
    finally { if (request.isLatest()) loading = false; }
  }

  $effect(() => { void load(params.get("revision") || "", params.get("doc") || ""); });
  onDestroy(() => { alive = false; requests.invalidate(); });

  function openDocument(entry) { void push(pictureURL(name, snapshot.id, entry.path)); }
  function startPublication() {
    publishing = true;
    publicationError = "";
    publication = JSON.stringify({ expected_version: workspace.version, expected_revision: workspace.current_revision, producer: "", overview: "overview.md", documents: [] }, null, 2);
  }

  async function publish() {
    busy = true;
    publicationError = "";
    try {
      if (new TextEncoder().encode(publication).length > 8 * 1024 * 1024) throw new Error("Publication exceeds 8 MiB.");
      let bundle;
      try { bundle = JSON.parse(publication); } catch { throw new Error("Enter a valid publication JSON object."); }
      const manifest = await api.publishBigPicture(name, bundle);
      if (!alive) return;
      publishing = false;
      successToast("Document tree published");
      await push(pictureURL(name, manifest.id, manifest.overview));
    } catch (e) { if (alive) publicationError = e.message; }
    finally { if (alive) busy = false; }
  }

  async function remove() {
    if (!confirm(`Delete "${workspace.title}" and all its published documents? Source repositories and connections will be kept.`)) return;
    busy = true;
    try {
      await api.deleteBigPicture(name, workspace.version);
      if (alive) { successToast("Big Picture deleted"); await push("/big-pictures"); }
    } catch (e) { if (alive) error = e.message; }
    finally { if (alive) busy = false; }
  }

  async function scrollToAnchor() {
    const anchor = params.get("anchor");
    if (!anchor) return;
    await tick();
    globalThis.document.getElementById(anchor)?.scrollIntoView({ block: "start" });
  }

  async function generate() {
    if (!confirm("Send selected source snapshots (which may contain sensitive configuration) to the configured documentation model? Only explicitly granted MCP resource URIs will be read; no external tools run.")) return;
    busy = true;
    error = "";
    try {
      await api.generateBigPicture(name);
      if (alive) successToast("Research queued — follow progress in Activity, then reload this workspace");
    } catch (e) { if (alive) error = e.message; }
    finally { if (alive) busy = false; }
  }
</script>

<a class="mb-3 inline-block text-[13px] text-dim hover:text-fg" href="/big-pictures" use:link>← Big Pictures</a>
{#if workspace}
  <div class="mb-4 flex flex-wrap items-start justify-between gap-3"><div class="min-w-0"><span class="text-[12px] text-faint">{workspace.namespace} · config v{workspace.version}</span><h2 class="mt-1 text-lg font-semibold">{workspace.title}</h2>{#if workspace.description}<p class="mt-1 text-[13px] text-dim">{workspace.description}</p>{/if}</div><button class="btn btn-primary max-w-full shrink-0" disabled={busy || loading} onclick={generate} title="Uses the documentation model. Bounded source snapshots, not a live-state audit.">Research & generate</button></div>
  <div class="mb-4 flex flex-wrap items-center gap-2 border-b border-line pb-3">
    <div class="flex flex-wrap gap-1" role="tablist" aria-label="Workspace views">
      {#each [["documents", "Documents"], ["sources", "Sources & prompt"], ["settings", "Settings"]] as [id, label] (id)}
        <button class="view-toggle whitespace-nowrap text-[13px]" class:view-toggle-active={tab === id} role="tab" aria-selected={tab === id} disabled={busy} onclick={() => { tab = id; }}>{label}</button>
      {/each}
    </div>
    <div class="ml-auto flex flex-wrap items-center gap-2">
      <a class="text-[12px] text-dim hover:text-fg" href="/activity" use:link>Activity →</a>
      <button class="btn btn-sm" disabled={busy} onclick={() => load(params.get("revision") || "", params.get("doc") || "")}>Reload</button>
      <button class="btn btn-sm" disabled={busy || loading} onclick={startPublication}>Publish documents</button>
      <button class="btn btn-sm btn-danger" disabled={busy || loading} onclick={remove}>Delete</button>
    </div>
  </div>
{/if}
{#if error}<p class="mb-3 text-danger" role="alert">{error}</p>{/if}
{#if workspace?.last_run}
  {@const run = workspace.last_run}
  <p class="mb-3 text-[12px] {run.status === 'failed' ? 'text-danger' : 'text-dim'}" role={run.status === "failed" ? "alert" : undefined}>
    Last {run.trigger === "schedule" ? "scheduled" : "manual"} research {fmtDate(run.at)}: {runStatusLabels[run.status] || run.status}{#if run.message} — {run.message}{/if}
  </p>
{/if}
{#if workspace?.current_revision}<p class="mb-3 text-[12px] text-faint">Current publication search indexes: lexical {workspace.text_revision === workspace.current_revision ? "ready" : "pending/unavailable"} · semantic {workspace.vector_revision === workspace.current_revision ? "ready" : "pending/unavailable"}. Semantic/hybrid search requires the docs embedder. {workspace.schedule?.length ? `Scheduled: ${workspace.schedule.join("; ")}` : "Updates are manual."}</p>{/if}
{#if publishing && workspace}
  <form class="card mb-4 space-y-3 p-4" onsubmit={(event) => { event.preventDefault(); publish(); }}>
    <h3 class="text-[15px] font-semibold">Publish an agent-produced document tree</h3>
    <p class="text-[12px] text-dim">Paste a complete publication JSON object. The overview must reference a document path. All documents are replaced together; the last 10 revisions remain readable. Citations are publisher-supplied, not independently verified.</p>
    <textarea class="input w-full font-mono text-[12px]" aria-label="Publication JSON" rows="12" bind:value={publication} disabled={busy}></textarea>
    {#if publicationError}<p class="text-danger" role="alert">{publicationError}</p>{/if}
    <div class="flex gap-2"><button class="btn btn-primary" type="submit" disabled={busy}>Publish</button><button class="btn" type="button" disabled={busy} onclick={() => { publishing = false; }}>Cancel</button></div>
  </form>
{/if}
{#if loading}<p class="text-dim">Loading…</p>
{:else if workspace}
  {#if tab === "settings"}
    {#key workspace.version}<BigPictureForm picture={workspace} onSaved={() => { tab = "documents"; void load(params.get("revision") || "", params.get("doc") || ""); }} onCancel={() => { tab = "documents"; }} />{/key}
  {:else if tab === "sources"}
    <div class="card space-y-3 p-4"><h3 class="text-[15px] font-semibold">Current research configuration</h3><p class="text-[12px] text-dim">These selections do not trigger source reads. Publications preserve the source selections and prompt from their configuration version.</p><ul class="space-y-2">{#each workspace.sources as source (`${source.kind}:${source.ref}`)}<li class="break-all font-mono text-[13px]">{source.kind}:{source.ref}</li>{/each}</ul><h4 class="text-[13px] font-semibold">Prompt</h4><pre class="whitespace-pre-wrap text-[13px]">{workspace.prompt}</pre></div>
  {:else if snapshot}
    {#if snapshot.change_summary}<p class="mb-3 text-[13px] text-dim">Changes: {snapshot.change_summary}</p>{/if}
    <div class="mb-3 flex flex-wrap items-center gap-3 text-[12px] text-dim"><label class="flex items-center gap-2">Publication<select class="input max-w-xs" value={snapshot.id} disabled={busy} onchange={(e) => push(pictureURL(name, e.currentTarget.value))}>{#each workspace.revisions as revision (revision.id)}<option value={revision.id}>{new Date(revision.published_at).toLocaleString()} · {revision.document_count} docs{revision.id === workspace.current_revision ? " · current" : ""}</option>{/each}</select></label><span>{snapshot.producer} · config v{snapshot.config_version}</span></div>
    {#if snapshot.config_version !== workspace.version}<p class="mb-3 text-[13px] text-danger">This publication uses an older configuration. Its sources and prompt may differ from the current workspace.</p>{/if}
    <div class="flex min-w-0 flex-col gap-3 lg:flex-row">
      <aside class="card shrink-0 p-2 lg:w-64"><FileTree entries={tree.entries} children={tree.children} {selected} {expanded} onToggle={(entry) => { expanded[entry.path] = !expanded[entry.path]; }} onOpen={openDocument} /></aside>
      <div class="card min-w-0 flex-1">
        {#if document}
          <div class="border-b border-line px-4 py-3"><h3 class="text-[15px] font-semibold">{document.title}</h3><p class="mt-1 font-mono text-[12px] text-faint">{selected}</p></div>
          <MarkdownView markdown={document.content} resolveLink={(href) => pictureDocumentLink(href, name, snapshot.id, selected, snapshot.documents)} onHeadings={scrollToAnchor} />
          <details class="border-t border-line p-4 text-[12px]"><summary class="cursor-pointer text-dim">Source citations ({document.evidence.length}) — publisher supplied</summary><ul class="mt-2 space-y-2">{#each document.evidence as evidence, index (index)}<li class="break-all font-mono">{evidence.source.kind}:{evidence.source.ref} · {evidence.locator}{evidence.revision ? ` @ ${evidence.revision}` : ""}</li>{/each}</ul></details>
        {/if}
      </div>
    </div>
  {:else if workspace.current_revision || params.get("revision")}
    <div class="card p-6"><h3 class="mb-2 text-[15px] font-semibold">Publication unavailable</h3><p class="text-[13px] text-dim">This revision could not be loaded. It may have expired or been deleted.</p><a class="btn mt-3" href={pictureURL(name, workspace.current_revision)} use:link>Open current publication</a></div>
  {:else}
    <div class="card p-6"><h3 class="mb-2 text-[15px] font-semibold">Workspace ready; no documents published yet</h3><p class="text-[13px] text-dim">Use “Research & generate” to collect bounded evidence and produce a document tree with your configured documentation model. Subsequent runs apply incremental patches and preserve untouched pages. Enable recurring updates with a schedule under Settings, or publish external agent output through <code>publish_big_picture</code>.</p><p class="mt-3 text-[12px] text-faint">Enable documentation generation and configure a chat model under Settings first. Documents are indexed after publication and searchable in the global Docs search.</p></div>
  {/if}
{/if}
