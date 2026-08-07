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
  import { api } from "../lib/api.js";
  import { successToast } from "../lib/toast.js";
  import { navigate } from "../lib/router.js";
  import Icon from "../lib/Icon.svelte";
  import Status from "../lib/Status.svelte";

  let { apiName = "" } = $props();

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

  let showAdd = $state(false);
  let editingName = $state("");
  let busy = $state(false);
  let testResult = $state(null);

  let groupForm = $state({ name: "", description: "" });

  const emptyForm = () => ({
    name: "",
    kind: "openapi",
    group: "",
    description: "",
    base_url: "",
    refresh_interval: "24h",
    schedule: "",
    spec_patch: "",
    operations: "",
    // openapi
    url: "",
    user: "",
    token: "",
    insecure_skip_verify: false,
    // grpc
    target: "",
    plaintext: false,
    server_name: "",
    grpc_services: "",
  });

  let form = $state(emptyForm());

  async function load() {
    try {
      const [g, s, k] = await Promise.all([api.apiGroups(), api.apiServices(), api.apiKinds()]);
      groups = g || [];
      services = (s && s.services) || [];
      kinds = (k && k.kinds) || [];
      error = "";
    } catch (e) {
      error = e.message;
    } finally {
      loaded = true;
    }
  }

  onMount(load);

  // Poll while anything is syncing so status and endpoint counts settle without
  // a manual reload; stop as soon as nothing is running.
  $effect(() => {
    if (!services.some((s) => s.running || s.status === "fetching" || s.task_state === "queued")) return;
    const id = setInterval(load, 2000);
    return () => clearInterval(id);
  });

  // Deep link: /apis/<name> opens that service expanded.
  $effect(() => {
    if (apiName && !expanded[apiName]) toggle(apiName);
  });

  const grouped = $derived.by(() => {
    const byGroup = new Map();
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
    try {
      const res = await api.apiService(name, { q: f.q, tag: f.tag, method: f.method });
      endpoints = { ...endpoints, [name]: res };
    } catch {
      // errorToast already fired in the api wrapper
    }
  }

  async function toggle(name) {
    expanded = { ...expanded, [name]: !expanded[name] };
    if (expanded[name] && !endpoints[name]) await loadEndpoints(name);
  }

  async function applyFilter(name, patch) {
    filters = { ...filters, [name]: { ...filterFor(name), ...patch } };
    await loadEndpoints(name);
  }

  async function openDetail(service, operationId) {
    detailBusy = true;
    try {
      const res = await api.apiOperation(service, operationId);
      detail = { service, operationId, ...res };
    } catch {
      detail = null;
    } finally {
      detailBusy = false;
    }
  }

  // ---- service form --------------------------------------------------------

  // providerConfig builds the opaque config the selected provider owns. A blank
  // secret is sent as an empty string, which the provider reads as "keep the
  // stored one" — the redacted config the UI holds never contains a token.
  function providerConfig() {
    if (form.kind === "grpc") {
      const cfg = {
        target: form.target.trim(),
        plaintext: form.plaintext,
        insecure_skip_verify: form.insecure_skip_verify,
        token: form.token,
      };
      if (form.server_name.trim()) cfg.server_name = form.server_name.trim();
      const list = form.grpc_services
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean);
      cfg.services = list.length ? list : null;
      return cfg;
    }
    return {
      url: form.url.trim(),
      user: form.user.trim(),
      token: form.token,
      insecure_skip_verify: form.insecure_skip_verify,
    };
  }

  // parseJSON returns [value, error]. An empty box means null, which clears the
  // stored value — the same rule the API uses.
  function parseJSON(raw, label) {
    const text = raw.trim();
    if (!text) return [null, ""];
    try {
      return [JSON.parse(text), ""];
    } catch (e) {
      return [null, `${label}: ${e.message}`];
    }
  }

  function serviceBody() {
    const [patch, patchErr] = parseJSON(form.spec_patch, "Spec patch");
    if (patchErr) throw new Error(patchErr);
    const [ops, opsErr] = parseJSON(form.operations, "Operation overrides");
    if (opsErr) throw new Error(opsErr);

    const specs = form.schedule
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);

    return {
      name: form.name.trim().toLowerCase(),
      kind: form.kind,
      group: form.group.trim(),
      description: form.description.trim(),
      base_url: form.base_url.trim(),
      refresh_interval: form.refresh_interval === "manual" ? "" : form.refresh_interval,
      specs,
      spec_patch: patch,
      operations: ops,
      config: providerConfig(),
    };
  }

  function startEdit(s) {
    editingName = s.name;
    showAdd = true;
    testResult = null;

    const cfg = s.config || {};
    form = {
      ...emptyForm(),
      name: s.name,
      kind: s.kind,
      group: s.effective_group === "default" ? "" : s.effective_group,
      description: s.description || "",
      base_url: s.base_url || "",
      refresh_interval: s.refresh_interval || "manual",
      schedule: (s.specs || []).join(", "),
      spec_patch: s.spec_patch ? JSON.stringify(s.spec_patch, null, 2) : "",
      operations: s.operations ? JSON.stringify(s.operations, null, 2) : "",
      url: cfg.url || "",
      user: cfg.user || "",
      token: "",
      insecure_skip_verify: !!cfg.insecure_skip_verify,
      target: cfg.target || "",
      plaintext: !!cfg.plaintext,
      server_name: cfg.server_name || "",
      grpc_services: (cfg.services || []).join(", "),
    };
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  function cancelEdit() {
    showAdd = false;
    editingName = "";
    testResult = null;
    form = emptyForm();
  }

  async function save() {
    busy = true;
    try {
      const body = serviceBody();
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
      const [patch, patchErr] = parseJSON(form.spec_patch, "Spec patch");
      if (patchErr) throw new Error(patchErr);
      testResult = await api.testApiConfig({
        kind: form.kind,
        existing_name: editingName,
        config: providerConfig(),
        spec_patch: patch,
      });
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

  const METHOD_COLORS = {
    GET: "text-ok",
    POST: "text-busy",
    PUT: "text-warn",
    PATCH: "text-warn",
    DELETE: "text-err",
    GRPC: "text-busy",
  };
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
      <div class="card flex flex-col gap-3 p-4">
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Name (search scope)
            <input class="input" placeholder="e.g. billing" bind:value={form.name} disabled={!!editingName} />
          </label>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Kind
            <select class="input" bind:value={form.kind} disabled={!!editingName}>
              {#each kinds as k (k)}
                <option value={k}>{k === "grpc" ? "gRPC (server reflection)" : "OpenAPI / Swagger"}</option>
              {/each}
            </select>
          </label>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Group
            <input class="input" placeholder="e.g. finance (blank = default)" bind:value={form.group} />
          </label>
        </div>

        {#if form.kind === "grpc"}
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Target (host:port)
              <input class="input font-mono" placeholder="billing.internal:443" bind:value={form.target} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              TLS server name (optional)
              <input class="input" bind:value={form.server_name} />
            </label>
          </div>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Services (optional, comma-separated; blank catalogues everything the server reports)
            <input class="input font-mono" placeholder="billing.v1.Invoices, billing.v1.Payments" bind:value={form.grpc_services} />
          </label>
          <label class="flex items-center gap-2 text-[13px] text-dim">
            <input type="checkbox" bind:checked={form.plaintext} />
            Plaintext (no TLS)
          </label>
        {:else}
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Document URL
              <input
                class="input font-mono"
                placeholder="https://docs.corp/billing/openapi.yaml"
                bind:value={form.url}
              />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Basic-auth user (optional)
              <input class="input" bind:value={form.user} />
            </label>
          </div>
        {/if}

        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Token {editingName ? "(leave blank to keep the stored one)" : "(optional)"}
            <input class="input" type="password" autocomplete="off" bind:value={form.token} />
          </label>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Auto refresh {form.schedule.trim() ? "(overridden by schedule)" : ""}
            <select class="input" bind:value={form.refresh_interval} disabled={!!form.schedule.trim()}>
              <option value="manual">manual only</option>
              <option value="1h">every hour</option>
              <option value="6h">every 6 hours</option>
              <option value="24h">daily</option>
              <option value="168h">weekly</option>
            </select>
          </label>
        </div>

        <label class="flex items-center gap-2 text-[13px] text-dim">
          <input type="checkbox" bind:checked={form.insecure_skip_verify} />
          Skip TLS verification (for servers behind a private CA)
        </label>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Cron schedule (optional; comma-separated, overrides auto refresh)
          <input class="input font-mono" placeholder="0 3 * * *,  @every 6h" bind:value={form.schedule} />
        </label>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Description (what this API is for — shown to MCP/AI; overrides the document's own)
          <input class="input" placeholder="e.g. Invoicing, payments and dunning" bind:value={form.description} />
        </label>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Base URL override
          <input class="input font-mono" placeholder="https://billing.internal.corp" bind:value={form.base_url} />
          <span class="text-[11px] text-faint">
            Replaces the servers the document declares. Set this when the published document names a
            host you cannot reach — every generated request uses it.
          </span>
        </label>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Spec patch (JSON Merge Patch, RFC 7386)
          <textarea
            class="input h-28 font-mono text-[12px]"
            placeholder={'{ "components": { "schemas": { "Money": { "properties": { "amount": { "type": "string" } } } } } }'}
            bind:value={form.spec_patch}
          ></textarea>
          <span class="text-[11px] text-faint">
            Applied to the raw document before parsing, so it can correct anything — schemas
            included. A <code class="font-mono">null</code> value deletes a key. Leave empty for none.
          </span>
        </label>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Endpoint overrides (JSON, keyed by operation id or "METHOD /path")
          <textarea
            class="input h-24 font-mono text-[12px]"
            placeholder={'{ "deleteAllInvoices": { "hidden": true }, "POST /v1/invoices": { "summary": "Raise an invoice" } }'}
            bind:value={form.operations}
          ></textarea>
          <span class="text-[11px] text-faint">
            Set <code class="font-mono">hidden</code> to drop an endpoint from the catalog entirely,
            or override its <code class="font-mono">summary</code>,
            <code class="font-mono">description</code> and <code class="font-mono">tags</code>.
          </span>
        </label>

        {#if testResult}
          <div class="rounded-md border border-line bg-surface-2 px-3 py-2 text-[12.5px]">
            <span class="font-medium">{testResult.title || "(untitled)"}</span>
            {#if testResult.version}<span class="text-faint"> v{testResult.version}</span>{/if}
            <span class="text-dim"> — {testResult.operation_count} endpoints</span>
            {#if testResult.base_url}
              <div class="font-mono text-[11px] text-faint">{testResult.base_url}</div>
            {/if}
            {#if testResult.sample?.length}
              <ul class="mt-1 font-mono text-[11px] text-faint">
                {#each testResult.sample as line (line)}<li>{line}</li>{/each}
              </ul>
            {/if}
          </div>
        {/if}

        <div class="flex gap-2">
          <button class="btn btn-primary" onclick={save} disabled={busy}>
            {editingName ? "Save changes" : "Add service"}
          </button>
          <button class="btn" onclick={test} disabled={busy}>Test &amp; preview</button>
          <button class="btn" onclick={cancelEdit} disabled={busy}>Cancel</button>
        </div>
      </div>
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
                            <span class="w-16 shrink-0 font-mono text-[11.5px] font-medium {METHOD_COLORS[op.method] || 'text-dim'}">
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
  <div
    class="fixed inset-0 z-40 flex justify-end bg-black/40"
    role="button"
    tabindex="-1"
    onclick={(e) => {
      if (e.target === e.currentTarget) detail = null;
    }}
    onkeydown={(e) => e.key === "Escape" && (detail = null)}
  >
    <div class="h-full w-full max-w-2xl overflow-y-auto bg-surface p-5 shadow-xl">
      {#if detailBusy}
        <div class="text-dim">Loading…</div>
      {:else if detail}
        {@const d = detail.detail || {}}
        <div class="mb-3 flex items-start gap-2">
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <span class="font-mono text-[13px] font-medium {METHOD_COLORS[d.method] || 'text-dim'}">{d.method}</span>
              <span class="min-w-0 break-all font-mono text-[13.5px]">{d.path}</span>
            </div>
            {#if d.summary}<div class="mt-1 text-[13px] text-dim">{d.summary}</div>{/if}
            <div class="mt-1 font-mono text-[11px] text-faint">{detail.operationId}</div>
          </div>
          <button class="icon-btn" onclick={() => (detail = null)} aria-label="Close">
            <Icon name="x" size={16} />
          </button>
        </div>

        {#if d.truncated}
          <div class="mb-3 rounded-md border border-warn bg-warn/10 px-3 py-2 text-[12px] text-warn">
            Some schema detail was omitted to keep this bounded; consult the source specification for
            the full definition.
          </div>
        {/if}

        {#if d.description}
          <p class="mb-3 whitespace-pre-wrap text-[13px] text-dim">{d.description}</p>
        {/if}

        {#if d.request?.command}
          <h3 class="mb-1 text-[13px] font-medium">Example request</h3>
          <pre class="mb-3 overflow-x-auto rounded-md bg-surface-2 p-3 font-mono text-[11.5px]">{d.request.command}</pre>
        {/if}

        {#if d.parameters?.length}
          <h3 class="mb-1 text-[13px] font-medium">Parameters</h3>
          <table class="mb-3 w-full text-left text-[12px]">
            <thead class="text-faint">
              <tr><th class="py-1">Name</th><th>In</th><th>Type</th><th>Required</th></tr>
            </thead>
            <tbody>
              {#each d.parameters as p (p.in + p.name)}
                <tr class="border-t border-line">
                  <td class="py-1 font-mono">{p.name}</td>
                  <td class="text-dim">{p.in}</td>
                  <td class="text-dim">{p.type || "—"}</td>
                  <td class="text-dim">{p.required ? "yes" : ""}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        {/if}

        {#if d.request_body?.schema}
          <h3 class="mb-1 text-[13px] font-medium">
            Request body <span class="font-mono text-[11px] text-faint">{d.request_body.content_type}</span>
          </h3>
          <pre class="mb-3 overflow-x-auto rounded-md bg-surface-2 p-3 font-mono text-[11.5px]">{JSON.stringify(
              d.request_body.schema,
              null,
              2,
            )}</pre>
        {/if}

        {#if d.responses?.length}
          <h3 class="mb-1 text-[13px] font-medium">Responses</h3>
          {#each d.responses as r (r.status)}
            <div class="mb-2">
              <div class="text-[12.5px]">
                <span class="font-mono font-medium">{r.status}</span>
                {#if r.description}<span class="text-dim"> — {r.description}</span>{/if}
              </div>
              {#if r.schema}
                <pre class="mt-1 max-h-64 overflow-auto rounded-md bg-surface-2 p-3 font-mono text-[11.5px]">{JSON.stringify(
                    r.schema,
                    null,
                    2,
                  )}</pre>
              {/if}
            </div>
          {/each}
        {/if}

        {#if d.notes?.length}
          <div class="mt-3 flex flex-col gap-1">
            {#each d.notes as note (note)}
              <div class="text-[12px] text-faint">{note}</div>
            {/each}
          </div>
        {/if}
      {/if}
    </div>
  </div>
{/if}
