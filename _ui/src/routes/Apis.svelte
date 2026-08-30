<script>
  // The API catalog: OpenAPI/Swagger documents and gRPC servers, filed into
  // groups. A group's description is what an LLM reads (via list_api_groups) to
  // pick where to look, so it is edited here rather than buried in a service.
  //
  // The page mirrors the catalog's own read order — group, service, endpoint,
  // detail — because that is the order a person answering "how do I call this"
  // actually needs, and because loading every endpoint of every service up
  // front is exactly what the catalog exists to avoid.
  import { onMount } from "svelte";
  import { SvelteMap } from "svelte/reactivity";
  import { api } from "../lib/api.js";
  import { createLatestRequest, createPoller } from "../lib/async.js";
  import { successToast } from "../lib/toast.js";
  import { path as routePath } from "../lib/router.js";
  import Icon from "../lib/Icon.svelte";
  import Status from "../lib/Status.svelte";
  import ApiServiceForm from "../components/ApiServiceForm.svelte";
  import ApiOperationDialog from "../components/ApiOperationDialog.svelte";
  import {
    apiServiceFormFromService,
    buildApiConfigTestPayload,
    buildApiServicePayload,
    createApiServiceForm,
  } from "../lib/api-service-config.js";
  import {
    API_METHOD_COLORS,
    buildOperationCallPayload,
    createOperationRequest,
  } from "../lib/api-operation.js";

  let { apiName = "" } = $props();

  // docParam carries a deep link from docs search: /apis/<name>?doc=<slug>.md.
  // The markdown projection is generated per endpoint and named after its
  // slug, which is also a handle the catalog can resolve, so a search hit
  // opens the endpoint's own detail view instead of the raw file.
  let docParam = $derived.by(() => {
    const params = new URLSearchParams($routePath.split("?")[1] || "");
    return params.get("doc") || "";
  });

  let groups = $state([]);
  let services = $state([]);
  let kinds = $state([]);
  let loaded = $state(false);
  let error = $state("");

  // Per-service expansion state, plus the lazily loaded endpoint page for each.
  let expanded = $state({});
  let endpoints = $state({});
  let filters = $state({});
  let detail = $state(null);
  let detailBusy = $state(false);
  let tryOpen = $state(false);
  let tryBusy = $state(false);
  let tryRes = $state(null);
  let tryReq = $state({ pathParams: [], query: [], headers: [], body: "" });
  const loadRequests = createLatestRequest();
  const detailRequests = createLatestRequest();
  const tryRequests = createLatestRequest();
  const endpointRequests = new SvelteMap();

  function endpointRequest(name) {
    if (!endpointRequests.has(name)) endpointRequests.set(name, createLatestRequest());
    return endpointRequests.get(name);
  }

  function closeDetail() {
    detailRequests.invalidate();
    invalidateTry();
    detail = null;
    detailBusy = false;
  }

  function invalidateTry() {
    tryRequests.invalidate();
    tryOpen = false;
    tryBusy = false;
    tryRes = null;
  }

  let showAdd = $state(false);
  let editingName = $state("");
  let busy = $state(false);
  let testResult = $state(null);

  let groupForm = $state({ name: "", description: "" });

  let form = $state(createApiServiceForm());

  async function load() {
    const isLatest = loadRequests.next();
    try {
      const [g, s, k] = await Promise.all([api.apiGroups(), api.apiServices(), api.apiKinds()]);
      if (!isLatest()) return;
      groups = g || [];
      services = (s && s.services) || [];
      kinds = (k && k.kinds) || [];
      error = "";
    } catch (e) {
      if (isLatest()) error = e.message;
    } finally {
      if (isLatest()) loaded = true;
    }
  }

  const catalogPoller = createPoller(load);
  onMount(() => {
    void catalogPoller.run();
    return () => {
      catalogPoller.stop();
      loadRequests.invalidate();
      detailRequests.invalidate();
      tryRequests.invalidate();
      for (const request of endpointRequests.values()) request.invalidate();
    };
  });

  // Poll while anything is syncing so status and endpoint counts settle without
  // a manual reload; stop as soon as nothing is running.
  $effect(() => {
    if (!services.some((s) => s.running || s.status === "fetching" || s.task_state === "queued")) {
      catalogPoller.stop();
      return;
    }
    catalogPoller.start(2000);
    return () => catalogPoller.stop();
  });

  // Deep link: /apis/<name> opens that service expanded.
  $effect(() => {
    if (apiName && !expanded[apiName]) toggle(apiName);
  });

  // ...and ?doc=<slug>.md additionally opens that endpoint. Tracked by link so
  // navigating between two hits in the same service re-opens the detail rather
  // than leaving the first one on screen.
  let openedDoc = "";
  let detailRoute = null;
  $effect(() => {
    const route = `${apiName}\u0000${docParam}`;
    const link = apiName && docParam ? route : "";
    if (detailRoute === route && openedDoc === link) return;
    detailRoute = route;
    closeDetail();
    openedDoc = link;
    if (link) void openDetail(apiName, docParam.replace(/\.md$/, ""));
  });

  const grouped = $derived.by(() => {
    const byGroup = new SvelteMap();
    for (const g of groups) byGroup.set(g.name, { ...g, services: [] });
    for (const s of services) {
      const key = s.effective_group || "default";
      if (!byGroup.has(key)) byGroup.set(key, { name: key, description: "", services: [] });
      byGroup.get(key).services.push(s);
    }
    return [...byGroup.values()].sort((a, b) => a.name.localeCompare(b.name));
  });

  function filterFor(name) {
    return filters[name] || { q: "", tag: "", method: "" };
  }

  async function loadEndpoints(name) {
    const f = filterFor(name);
    const isLatest = endpointRequest(name).next();
    try {
      const res = await api.apiService(name, { q: f.q, tag: f.tag, method: f.method });
      if (isLatest()) endpoints = { ...endpoints, [name]: res };
    } catch {
      // errorToast already fired in the api wrapper
    }
  }

  async function toggle(name) {
    expanded = { ...expanded, [name]: !expanded[name] };
    if (!expanded[name]) endpointRequest(name).invalidate();
    if (expanded[name] && !endpoints[name]) await loadEndpoints(name);
  }

  async function applyFilter(name, patch) {
    filters = { ...filters, [name]: { ...filterFor(name), ...patch } };
    await loadEndpoints(name);
  }

  async function openDetail(service, operationId) {
    const { isLatest, signal } = detailRequests.nextAbortable();
    invalidateTry();
    detail = null;
    detailBusy = true;
    try {
      const res = await api.apiOperation(service, operationId, { signal });
      if (!isLatest()) return;
      detail = { service, operationId, ...res };
      initTry(detail.detail || {});
    } catch (e) {
      if (isLatest() && e?.name !== "AbortError") detail = null;
    } finally {
      if (isLatest()) detailBusy = false;
    }
  }

  // ---- try it ---------------------------------------------------------------

  // The try panel is pre-filled from the operation itself: one row per declared
  // path/query parameter (example value when the spec gives one) and the
  // generated example body. The caller edits values, never the request shape —
  // that is the same boundary the backend enforces.
  function initTry(d) {
    tryReq = createOperationRequest(d);
    tryRes = null;
  }

  async function sendTry() {
    if (!detail) return;
    const selectedDetail = detail;
    const { isLatest, signal } = tryRequests.nextAbortable();
    tryBusy = true;
    tryRes = null;
    try {
      const result = await api.callApiOperation(
        selectedDetail.service,
        buildOperationCallPayload(selectedDetail.operationId, tryReq),
        { signal },
      );
      if (isLatest()) tryRes = result;
    } catch (e) {
      // A request krabby refused to assemble (missing parameter, bad JSON body).
      if (isLatest() && e?.name !== "AbortError") {
        tryRes = { status_text: "not sent", error: e.message, ok: false };
      }
    } finally {
      if (isLatest()) tryBusy = false;
    }
  }

  // ---- service form --------------------------------------------------------

  function startEdit(s) {
    editingName = s.name;
    showAdd = true;
    testResult = null;

    form = apiServiceFormFromService(s);
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  function cancelEdit() {
    showAdd = false;
    editingName = "";
    testResult = null;
    form = createApiServiceForm();
  }

  async function save() {
    busy = true;
    try {
      const body = buildApiServicePayload(form);
      if (editingName) {
        await api.updateApiService(editingName, body);
        successToast(`Updated ${editingName}`);
      } else {
        await api.addApiService(body);
        successToast(`Added ${body.name}; syncing in the background`);
      }
      cancelEdit();
      await load();
    } catch (e) {
      if (e?.message) error = e.message;
    } finally {
      busy = false;
    }
  }

  async function test() {
    busy = true;
    testResult = null;
    try {
      testResult = await api.testApiConfig(buildApiConfigTestPayload(form, editingName));
    } catch (e) {
      if (e?.message) error = e.message;
    } finally {
      busy = false;
    }
  }

  async function refresh(name, force) {
    await api.refreshApiService(name, force);
    successToast(force ? `Re-rendering ${name}` : `Syncing ${name}`);
    await load();
  }

  async function remove(name) {
    if (!confirm(`Delete API service "${name}" and its indexed endpoints?`)) return;
    await api.deleteApiService(name);
    successToast(`Deleted ${name}`);
    delete endpoints[name];
    await load();
  }

  async function saveGroup() {
    const name = groupForm.name.trim();
    if (!name) return;
    await api.upsertApiGroup({ name, description: groupForm.description.trim() });
    successToast(`Saved group ${name}`);
    groupForm = { name: "", description: "" };
    await load();
  }

  async function removeGroup(name) {
    if (!confirm(`Delete the description of group "${name}"? Services keep their tag.`)) return;
    await api.deleteApiGroup(name);
    await load();
  }

  function editGroup(g) {
    groupForm = { name: g.name, description: g.description || "" };
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

</script>

<div class="flex flex-col gap-4">
  <p class="text-[13px] text-dim">
    OpenAPI/Swagger documents and gRPC servers, catalogued as groups of services. Each endpoint is
    stored with its parameters, schemas and a ready-to-run command, and indexed for search under
    <code class="font-mono">api:&lt;name&gt;</code>. When a published document is wrong for your
    environment, override its base URL, patch the document itself, or correct individual endpoints.
  </p>

  {#if error}
    <div class="rounded-md border border-err bg-err/10 px-3 py-2.5 text-[13px] text-err">
      {error}
      <button class="ml-2 underline" onclick={() => (error = "")}>dismiss</button>
    </div>
  {/if}

  <!-- Group descriptions -->
  <div class="card p-4">
    <h2 class="text-[13.5px] font-medium">Groups</h2>
    <p class="mt-1 text-[12px] text-faint">
      A group's description is what an agent reads to decide which APIs are relevant. Groups are
      created by naming one on a service; this only sets the description.
    </p>

    <div class="mt-3 flex flex-col gap-2 sm:flex-row">
      <input class="input sm:w-52" placeholder="group name, e.g. finance" bind:value={groupForm.name} />
      <input
        class="input flex-1"
        placeholder="what this group holds, e.g. invoicing, payments and dunning APIs"
        bind:value={groupForm.description}
      />
      <button class="btn btn-primary" onclick={saveGroup} disabled={!groupForm.name.trim()}>Save</button>
    </div>

    {#if groups.length}
      <div class="mt-3 flex flex-col gap-1.5">
        {#each groups as g (g.name)}
          <div class="flex items-center gap-2 text-[13px]">
            <span class="font-mono font-medium">{g.name}</span>
            <span class="text-faint">({g.service_count})</span>
            <span class="min-w-0 flex-1 truncate text-dim">{g.description || "—"}</span>
            <button class="icon-btn" title="Edit description" onclick={() => editGroup(g)}>
              <Icon name="settings" size={13} />
            </button>
            <button class="icon-btn" title="Delete description" onclick={() => removeGroup(g.name)}>
              <Icon name="trash" size={13} />
            </button>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <!-- Add / edit a service -->
  <div>
    {#if !showAdd}
      <button class="btn btn-primary" onclick={() => (showAdd = true)}>Add API service</button>
    {:else}
      <ApiServiceForm
        bind:form
        {editingName}
        {kinds}
        {busy}
        {testResult}
        onSave={save}
        onTest={test}
        onCancel={cancelEdit}
      />
    {/if}
  </div>

  <!-- Catalog -->
  <div class="flex flex-col gap-4">
    {#if !loaded}
      <div class="card p-6 text-center text-dim">Loading…</div>
    {:else if services.length === 0}
      <div class="card p-6 text-center text-dim">No API services yet.</div>
    {:else}
      {#each grouped as g (g.name)}
        {#if g.services.length}
          <div class="flex flex-col gap-2">
            <div class="flex items-baseline gap-2 px-1">
              <h2 class="font-mono text-[13.5px] font-medium">{g.name}</h2>
              {#if g.description}
                <span class="text-[12px] text-faint">{g.description}</span>
              {/if}
            </div>

            {#each g.services as s (s.name)}
              <div class="card overflow-hidden">
                <div class="flex items-center hover:bg-surface-2">
                  <button
                    class="flex min-w-0 flex-1 cursor-pointer items-center gap-2.5 px-3.5 py-2.5 text-left"
                    onclick={() => toggle(s.name)}
                    aria-expanded={!!expanded[s.name]}
                  >
                    <Icon name={expanded[s.name] ? "chevron-down" : "chevron-right"} size={14} />
                    <Icon name="braces" size={14} />
                    <span class="font-mono text-[13.5px] font-medium">{s.name}</span>
                    <span class="rounded border border-line px-1.5 text-[11px] text-dim">{s.kind}</span>
                    <span class="font-mono text-[11px] text-faint">api:{s.name}</span>
                    {#if s.title}<span class="truncate text-[12px] text-dim">{s.title}</span>{/if}
                    <span class="ml-auto flex items-center gap-2.5 text-[12px] text-faint">
                      {#if s.running}
                        <span class="text-busy">({s.running})</span>
                      {:else if s.task_state === "queued"}
                        <span class="text-warn">(queued)</span>
                      {/if}
                      <span>{s.operation_count} {s.operation_count === 1 ? "endpoint" : "endpoints"}</span>
                      <Status status={s.status} />
                    </span>
                  </button>
                </div>

                {#if expanded[s.name]}
                  <div class="border-t border-line px-3.5 py-3">
                    {#if s.last_error}
                      <div class="mb-2 rounded-md border border-err bg-err/10 px-2.5 py-1.5 text-[12px] text-err">
                        {s.last_error}
                      </div>
                    {/if}

                    <div class="mb-2 flex flex-wrap items-center gap-2 text-[12px] text-faint">
                      {#if s.base_url}
                        <span class="font-mono">{s.base_url}</span>
                        {#if s.base_url && s.resolved_base_url && s.base_url !== s.resolved_base_url}
                          <span class="rounded border border-line px-1.5 text-[11px] text-warn">base URL overridden</span>
                        {/if}
                      {/if}
                      {#if s.spec_patch}
                        <span class="rounded border border-line px-1.5 text-[11px] text-warn">spec patched</span>
                      {/if}
                      {#if s.operations}
                        <span class="rounded border border-line px-1.5 text-[11px] text-warn">
                          {Object.keys(s.operations).length} endpoint overrides
                        </span>
                      {/if}
                    </div>

                    <div class="mb-3 flex flex-wrap gap-2">
                      <button class="btn btn-sm" onclick={() => refresh(s.name, false)}>Sync</button>
                      <button
                        class="btn btn-sm"
                        title="Re-render every endpoint from the same document, for when an override changed"
                        onclick={() => refresh(s.name, true)}>Re-render</button
                      >
                      <button class="btn btn-sm" onclick={() => startEdit(s)}>Edit</button>
                      <button class="btn btn-sm" onclick={() => remove(s.name)}>Delete</button>
                    </div>

                    <!-- Endpoint filters -->
                    <div class="mb-2 flex flex-wrap gap-2">
                      <input
                        class="input h-8 max-w-64 flex-1 text-[12.5px]"
                        placeholder="filter by path, summary or operation id"
                        value={filterFor(s.name).q}
                        onchange={(e) => applyFilter(s.name, { q: e.currentTarget.value })}
                      />
                      <select
                        class="input h-8 w-36 text-[12.5px]"
                        value={filterFor(s.name).method}
                        onchange={(e) => applyFilter(s.name, { method: e.currentTarget.value })}
                      >
                        <option value="">any method</option>
                        {#each ["GET", "POST", "PUT", "PATCH", "DELETE", "GRPC"] as m (m)}
                          <option value={m}>{m}</option>
                        {/each}
                      </select>
                      {#if endpoints[s.name]?.tags?.length}
                        <select
                          class="input h-8 w-44 text-[12.5px]"
                          value={filterFor(s.name).tag}
                          onchange={(e) => applyFilter(s.name, { tag: e.currentTarget.value })}
                        >
                          <option value="">any tag</option>
                          {#each endpoints[s.name].tags as t (t)}
                            <option value={t}>{t}</option>
                          {/each}
                        </select>
                      {/if}
                    </div>

                    {#if !endpoints[s.name]}
                      <div class="py-4 text-center text-[13px] text-dim">Loading endpoints…</div>
                    {:else if !endpoints[s.name].operations?.length}
                      <div class="py-4 text-center text-[13px] text-dim">No endpoints match.</div>
                    {:else}
                      <div class="flex flex-col divide-y divide-line">
                        {#each endpoints[s.name].operations as op (op.id)}
                          <button
                            class="flex items-center gap-2.5 py-1.5 text-left hover:bg-surface-2"
                            onclick={() => openDetail(s.name, op.operation_id)}
                          >
                            <span class="w-16 shrink-0 font-mono text-[11.5px] font-medium {API_METHOD_COLORS[op.method] || 'text-dim'}">
                              {op.method}
                            </span>
                            <span class="min-w-0 truncate font-mono text-[12.5px]">{op.path}</span>
                            {#if op.summary}
                              <span class="min-w-0 flex-1 truncate text-[12px] text-faint">{op.summary}</span>
                            {/if}
                            {#if op.deprecated}
                              <span class="shrink-0 rounded border border-line px-1.5 text-[11px] text-warn">deprecated</span>
                            {/if}
                          </button>
                        {/each}
                      </div>

                      {#if endpoints[s.name].has_more}
                        <div class="pt-2 text-center text-[12px] text-faint">
                          Showing {endpoints[s.name].operations.length} of {endpoints[s.name].total} — narrow with a filter.
                        </div>
                      {/if}
                    {/if}
                  </div>
                {/if}
              </div>
            {/each}
          </div>
        {/if}
      {/each}
    {/if}
  </div>
</div>

<!-- Endpoint detail -->
{#if detail || detailBusy}
  <ApiOperationDialog
    {detail}
    {detailBusy}
    bind:tryOpen
    bind:tryRequest={tryReq}
    tryBusy={tryBusy}
    tryResult={tryRes}
    onClose={closeDetail}
    onSend={sendTry}
  />
{/if}
