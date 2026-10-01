<script>
  import { onMount, onDestroy, untrack } from "svelte";
  import { api } from "../lib/api.js";
  import { createLatestRequest } from "../lib/async.js";
  import { createPictureDraft, picturePayload } from "../lib/big-picture.js";
  import { successToast } from "../lib/toast.js";

  let { picture = null, onSaved, onCancel } = $props();
  let draft = $state(createPictureDraft(untrack(() => picture)));
  let busy = $state(false);
  let error = $state("");
  let sourceKind = $state("repo");
  let sourceQuery = $state("");
  let sourcePage = $state(1);
  let options = $state({ items: [], total: 0, page: 1, per_page: 20 });
  let loadingSources = $state(false);
  let sourceError = $state("");
  let scheduleConsent = $state(false);
  const savedSchedule = untrack(() => (picture?.schedule || []).join("\n"));
  // Consent covers a schedule the user is enabling or changing, not one that
  // was already authorized when it was saved.
  let scheduleNeedsConsent = $derived(Boolean(draft.schedule_text.trim()) && scheduleKey(draft.schedule_text) !== scheduleKey(savedSchedule));
  function scheduleKey(text) {
    return text.split("\n").map((spec) => spec.trim()).filter(Boolean).join("\n");
  }
  const requests = createLatestRequest();

  async function loadSources(page = 1) {
    const request = requests.nextAbortable();
    sourcePage = page;
    loadingSources = true;
    sourceError = "";
    options = { items: [], total: 0, page, per_page: 20 };
    try {
      const response = await api.bigPictureSourceOptions(sourceKind, sourceQuery, page, request.signal);
      if (request.isLatest()) options = response;
    } catch (e) { if (request.isLatest() && e.name !== "AbortError") sourceError = e.message; }
    finally { if (request.isLatest()) loadingSources = false; }
  }

  function selected(option) { return draft.sources.some((s) => s.kind === option.kind && s.ref === option.ref); }
  function toggleSource(option, checked) {
    draft.sources = checked ? [...draft.sources, { kind: option.kind, ref: option.ref }] : draft.sources.filter((s) => s.kind !== option.kind || s.ref !== option.ref);
  }

  async function save() {
    if (busy) return;
    if (scheduleNeedsConsent && !scheduleConsent) {
      error = "Confirm recurring source disclosure before saving a schedule.";
      return;
    }
    busy = true;
    error = "";
    try {
      const result = picture ? await api.updateBigPicture(picture.name, picturePayload(draft)) : await api.addBigPicture(picturePayload(draft));
      successToast("Big Picture settings saved");
      onSaved(result);
    } catch (e) { error = e.message; }
    finally { busy = false; }
  }

  onMount(() => { void loadSources(); });
  onDestroy(() => requests.invalidate());
</script>

<form class="card space-y-4 p-4" onsubmit={(event) => { event.preventDefault(); save(); }}>
  <fieldset disabled={busy} class="space-y-4">
    <legend class="mb-3 text-[15px] font-semibold">{picture ? "Workspace settings" : "New Big Picture"}</legend>
    <div class="grid gap-3 sm:grid-cols-3">
      <label class="space-y-1 text-[13px]"><span>Name</span><input class="input w-full" required pattern="[a-z0-9][a-z0-9._-]*" maxlength="128" readonly={Boolean(picture)} bind:value={draft.name} placeholder="commerce-production" /></label>
      <label class="space-y-1 text-[13px]"><span>Title</span><input class="input w-full" required maxlength="256" bind:value={draft.title} placeholder="Commerce architecture" /></label>
      <label class="space-y-1 text-[13px]"><span>Namespace</span><input class="input w-full" required pattern="[a-z0-9][a-z0-9._-]*" maxlength="128" bind:value={draft.namespace} placeholder="commerce" /></label>
    </div>
    <p class="text-[12px] text-faint">Namespaces group Big Pictures, independently of repository tags. Sources can be reused across workspaces. A namespace is not an access-control boundary.</p>
    <label class="block space-y-1 text-[13px]"><span>Description</span><textarea class="input w-full" rows="2" maxlength="4096" bind:value={draft.description}></textarea></label>
    <label class="block space-y-1 text-[13px]"><span>Research prompt</span><textarea class="input w-full" required rows="6" maxlength="32768" bind:value={draft.prompt} placeholder="Explain production deployment, service calls and Kafka event flows. Cite sources and put detailed flows on separate pages."></textarea></label>
    <label class="block space-y-1 text-[13px]"><span>Scheduled incremental updates (optional)</span><textarea class="input w-full font-mono text-[12px]" rows="2" bind:value={draft.schedule_text} placeholder="0 2 * * *&#10;or @every 6h"></textarea></label>
    <p class="text-[12px] text-faint">One cron specification per line, up to 10. Empty disables scheduling. Uses locally synced repo/Source/API snapshots and currently granted MCP resource URIs; it does not refresh upstream sources. Unchanged collected evidence skips the model; updates preserve untouched pages.</p>
    {#if scheduleNeedsConsent}<label class="flex items-start gap-2 text-[12px] text-dim"><input type="checkbox" bind:checked={scheduleConsent} /><span>I authorize recurring source reads and disclosure of source snapshots to the configured model/trace services. Sources can contain sensitive configuration.</span></label>{/if}

    <section class="space-y-3">
      <h3 class="text-[13px] font-semibold">Sources ({draft.sources.length}/50)</h3>
      <div class="flex flex-wrap gap-2">
        {#each draft.sources as source (`${source.kind}:${source.ref}`)}
          <button class="btn btn-sm max-w-full" type="button" title="Remove source" onclick={() => toggleSource(source, false)}><span class="truncate">{source.kind}:{source.ref}</span><span aria-hidden="true">×</span></button>
        {/each}
      </div>
      <div class="flex flex-wrap gap-2">
        <select class="input" aria-label="Source kind" bind:value={sourceKind} onchange={() => loadSources()}><option value="repo">Repositories</option><option value="namespace">Repository namespaces</option><option value="bigpicture">Big Pictures</option><option value="web">Sources</option><option value="api">API catalog</option><option value="mcp">External MCPs</option></select>
        <input class="input min-w-0 flex-1" aria-label="Search sources" bind:value={sourceQuery} placeholder="Find sources…" onkeydown={(event) => { if (event.key === "Enter") { event.preventDefault(); loadSources(); } }} />
        <button class="btn" type="button" onclick={() => loadSources()}>Search</button>
      </div>
      {#if sourceKind === "mcp"}<p class="text-[12px] text-faint">Connection references only. Selecting an MCP does not read its contents or widen its saved tool/resource grants. Configure grants under Settings → External MCPs.</p>{/if}
      {#if sourceKind === "namespace"}<p class="text-[12px] text-faint">Select all repositories in a namespace as one source, including repositories added later. Research uses locally synced snapshots and shares the bounded text budget across repositories. Namespaces are not access-control boundaries.</p>{/if}
      {#if sourceKind === "bigpicture"}<p class="text-[12px] text-faint">Build a higher-level picture from other Big Pictures’ published documents. Each run reads their latest published revisions, not their original sources. Publish child pictures before research; self-references and circular dependencies are not allowed.</p>{/if}
      {#if sourceError}<p class="text-danger" role="alert">{sourceError}</p>{/if}
      {#if loadingSources}<p class="text-[13px] text-dim">Loading sources…</p>
      {:else}
        <div class="max-h-64 space-y-1 overflow-y-auto rounded border border-line p-2">
          {#each options.items as option (`${option.kind}:${option.ref}`)}
            <label class="flex items-start gap-2 rounded p-2 text-[13px] hover:bg-surface-2"><input type="checkbox" checked={selected(option)} disabled={(option.kind === "bigpicture" && option.ref === draft.name.trim()) || (!selected(option) && draft.sources.length >= 50)} onchange={(e) => toggleSource(option, e.currentTarget.checked)} /><span class="min-w-0"><span class="block break-all font-mono">{option.ref}</span><span class="block text-[12px] text-dim">{option.title !== option.ref ? option.title : ""} {option.namespace || ""} · {option.status || "unknown"}</span></span></label>
          {:else}<p class="p-2 text-[13px] text-dim">No matching sources. Try another search or add repositories, Big Pictures, indexed sources, APIs or external MCP connections first.</p>{/each}
        </div>
        <div class="flex items-center justify-between text-[12px] text-dim"><span>{options.total} matches · page {sourcePage}</span><div class="flex gap-2"><button class="btn btn-sm" type="button" disabled={sourcePage <= 1} onclick={() => loadSources(sourcePage - 1)}>Previous</button><button class="btn btn-sm" type="button" disabled={sourcePage * options.per_page >= options.total} onclick={() => loadSources(sourcePage + 1)}>Next</button></div></div>
      {/if}
    </section>
    {#if error}<p class="text-danger" role="alert">{error}</p>{/if}
    <div class="flex gap-2"><button class="btn btn-primary" type="submit" disabled={draft.sources.length === 0 || (scheduleNeedsConsent && !scheduleConsent)}>{picture ? "Save settings" : "Create Big Picture"}</button><button class="btn" type="button" onclick={onCancel}>Cancel</button></div>
  </fieldset>
  <p class="text-[12px] text-faint">Manual research uses “Research & generate” or generate_big_picture on the admin MCP. Saving a schedule enables recurring incremental updates. External tools are never executed.</p>
</form>
