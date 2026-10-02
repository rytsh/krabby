<script>
  import { onMount, onDestroy } from "svelte";
  import { api } from "../../lib/api.js";
  import { successToast } from "../../lib/toast.js";
  import { createExternalMCPDraft, buildExternalMCPPayload, identifierList } from "../../lib/external-mcp-config.js";

  let connections = $state([]);
  let saved = $state(null);
  let draft = $state(createExternalMCPDraft());
  let open = $state(false);
  let loading = $state(true);
  let busy = $state(false);
  let error = $state("");
  let discovery = $state(null);
  let allowedToolText = $state("");
  let allowedResourceText = $state("");
  let controller;
  let generation = 0;
  let changedURL = $derived(Boolean(saved && saved.url !== draft.url.trim()));

  // A catalog belongs to the tested endpoint and credentials, not the draft
  // being edited afterwards. Grants alone do not invalidate the catalog.
  $effect(() => {
    JSON.stringify([draft.url, draft.timeout_seconds, draft.bearer_token, draft.clear_bearer_token, draft.headers]);
    discovery = null;
  });

  onMount(async () => {
    try { connections = await api.externalMCPs(); }
    catch (e) { error = e.message; }
    finally { loading = false; }
  });
  onDestroy(() => { generation++; controller?.abort(); });

  function edit(connection = null) {
    saved = connection;
    draft = createExternalMCPDraft(connection);
    allowedToolText = draft.allowed_tools.join("\n");
    allowedResourceText = draft.allowed_resources.join("\n");
    discovery = null;
    error = "";
    open = true;
  }

  function toggle(field, value, checked) {
    const values = identifierList(field === "allowed_tools" ? allowedToolText : allowedResourceText);
    const text = (checked ? [...new Set([...values, value])] : values.filter((v) => v !== value)).join("\n");
    if (field === "allowed_tools") allowedToolText = text;
    else allowedResourceText = text;
  }

  function payload() {
    return buildExternalMCPPayload({ ...draft, allowed_tools: identifierList(allowedToolText), allowed_resources: identifierList(allowedResourceText) }, saved);
  }

  async function save() {
    busy = true;
    error = "";
    try {
      const config = payload();
      if (saved) await api.updateExternalMCP(saved.name, config);
      else await api.addExternalMCP(config);
      // Drop draft secrets as soon as the write succeeds, even if reload fails.
      draft = createExternalMCPDraft();
      open = false;
      discovery = null;
      connections = await api.externalMCPs();
      successToast("External MCP connection saved");
    } catch (e) { error = e.message; }
    finally { busy = false; }
  }

  async function discover() {
    busy = true;
    error = "";
    discovery = null;
    controller = new AbortController();
    const current = generation;
    try {
      const result = await api.discoverExternalMCP({ ...payload(), existing_name: saved?.name || "" }, controller.signal);
      if (current === generation) discovery = result;
    } catch (e) { if (e.name !== "AbortError" && current === generation) error = e.message; }
    finally { if (current === generation) busy = false; }
  }

  async function remove(connection) {
    if (!confirm(`Delete external MCP connection "${connection.name}"?`)) return;
    busy = true;
    error = "";
    try {
      await api.deleteExternalMCP(connection.name);
      connections = await api.externalMCPs();
      if (saved?.name === connection.name) { open = false; draft = createExternalMCPDraft(); discovery = null; }
    } catch (e) { error = e.message; }
    finally { busy = false; }
  }
</script>

<div class="mb-4 flex items-start justify-between gap-3">
  <div>
    <h2 class="mb-1 text-[15px] font-semibold">External MCPs</h2>
    <p class="text-dim">Outbound Streamable HTTP connections for future Big Picture research. These are separate from Krabby's own MCP catalogs.</p>
  </div>
  <button class="btn btn-primary shrink-0" disabled={busy || loading} onclick={() => edit()}>Add connection</button>
</div>
<p class="mb-4 text-[12px] text-faint">Administrator-only configuration: connections can reach Krabby's internal network. Use trusted endpoints. HTTPS protects credentials in transit; credentials are write-only and stored in the data volume. OAuth and local commands are not supported yet.</p>

{#if error}<div class="mb-3 text-danger" role="alert">{error}</div>{/if}
{#if loading}
  <p class="text-dim">Loading…</p>
{:else}
  <div class="card mb-4 overflow-x-auto">
    <table class="w-full text-left text-[13px]">
      <thead><tr class="border-b border-line text-dim"><th class="p-3">Name</th><th class="p-3">Endpoint</th><th class="p-3">Grants</th><th class="p-3">Status</th><th class="p-3"></th></tr></thead>
      <tbody>
        {#each connections as connection (connection.name)}
          <tr class="border-b border-line">
            <td class="p-3 font-mono">{connection.name}</td>
            <td class="break-all p-3">{connection.url}</td>
            <td class="p-3">{connection.allowed_tools.length} tools / {connection.allowed_resources.length} resources</td>
            <td class="p-3">{connection.disabled ? "Disabled" : "Enabled"}</td>
            <td class="p-3"><div class="flex justify-end gap-2"><button class="btn btn-sm" disabled={busy} onclick={() => edit(connection)}>Edit / test</button><button class="btn btn-sm btn-danger" disabled={busy} onclick={() => remove(connection)}>Delete</button></div></td>
          </tr>
        {:else}<tr><td colspan="5" class="p-6 text-center text-dim">No external MCP connections configured.</td></tr>{/each}
      </tbody>
    </table>
  </div>
{/if}

{#if open}
  <form class="card p-4" onsubmit={(event) => { event.preventDefault(); save(); }}>
    <fieldset disabled={busy} class="space-y-4">
      <legend class="mb-3 text-[15px] font-semibold">{saved ? `Edit ${saved.name}` : "New connection"}</legend>
      <div class="grid gap-3 sm:grid-cols-2">
        <label class="space-y-1 text-[13px]"><span>Name</span><input class="input w-full" required pattern="[a-z0-9][a-z0-9._-]*" maxlength="128" readonly={Boolean(saved)} bind:value={draft.name} placeholder="platform-config" /></label>
        <label class="space-y-1 text-[13px]"><span>Timeout (seconds)</span><input class="input w-full" type="number" min="1" max="120" required bind:value={draft.timeout_seconds} /></label>
      </div>
      <label class="block space-y-1 text-[13px]"><span>MCP endpoint</span><input class="input w-full" type="url" required bind:value={draft.url} placeholder="https://mcp.example.com/mcp" /><span class="block text-[12px] text-faint">HTTP(S), without URL credentials, query parameters or fragment. Use headers for authentication.</span></label>
      {#if changedURL}<p class="text-[13px] text-danger">Changing the endpoint clears saved credentials and grants. Enter new credentials, save, then rediscover and grant access.</p>{/if}
      <label class="block space-y-1 text-[13px]"><span>Description</span><textarea class="input w-full" rows="2" maxlength="4096" bind:value={draft.description}></textarea></label>
      <label class="block space-y-1 text-[13px]"><span>Bearer token</span><input class="input w-full" type="password" autocomplete="new-password" bind:value={draft.bearer_token} placeholder={saved?.bearer_token_set && !changedURL ? "Saved — blank keeps existing token" : "Optional token"} /></label>
      <label class="flex items-center gap-2 text-[13px]"><input type="checkbox" bind:checked={draft.clear_bearer_token} />Clear saved bearer token</label>
      <div class="space-y-2">
        <h3 class="text-[13px] font-medium">Custom headers (values are write-only)</h3>
        {#each draft.headers as header, index (header)}
          <div class="flex gap-2">
            <input class="input min-w-0 flex-1" aria-label={`Header ${index + 1} name`} bind:value={header.name} placeholder="X-API-Key" />
            <input class="input min-w-0 flex-1" type="password" autocomplete="new-password" aria-label={`Header ${index + 1} value`} bind:value={header.value} placeholder="Blank keeps matching saved value" />
            <button class="btn btn-sm" type="button" onclick={() => { draft.headers = draft.headers.filter((_, i) => i !== index); }}>Remove</button>
          </div>
        {/each}
        <button class="btn btn-sm" type="button" onclick={() => { draft.headers = [...draft.headers, { name: "", value: "" }]; }}>Add header</button>
      </div>
      <label class="flex items-center gap-2 text-[13px]"><input type="checkbox" bind:checked={draft.disabled} />Disable for research jobs (manual discovery still works)</label>
      <div class="grid gap-3 sm:grid-cols-2">
        <label class="block space-y-1 text-[13px]"><span>Allowed tool names — one per line</span><textarea class="input w-full font-mono" rows="3" bind:value={allowedToolText}></textarea></label>
        <label class="block space-y-1 text-[13px]"><span>Allowed resource URIs / templates — one per line</span><textarea class="input w-full font-mono" rows="3" bind:value={allowedResourceText}></textarea></label>
      </div>
      <p class="text-[12px] text-faint">No grants by default. Big Picture research reads granted resources and may call granted tools with values found in repositories, so grant only read-only tools and avoid tools that return secrets. Read-only hints are not proof of safety. This screen never executes tools or reads resource contents.</p>
      <div class="flex flex-wrap gap-2">
        <button class="btn" type="button" onclick={discover}>Test connection & discover</button>
        <button class="btn btn-primary" type="submit">Save connection</button>
        <button class="btn" type="button" onclick={() => { open = false; draft = createExternalMCPDraft(); discovery = null; }}>Cancel</button>
      </div>
    </fieldset>
    {#if busy}<p class="mt-3 text-[13px] text-dim">Working…</p>{/if}
  </form>
{/if}

{#if open && discovery}
  <div class="card mt-4 space-y-3 p-4">
    {#if !discovery.ok}<p class="text-danger" role="alert">{discovery.error}</p>
    {:else}
      <h3 class="text-[15px] font-semibold">{discovery.server?.name || "MCP server"} {discovery.server?.version || ""}</h3>
      <p class="text-[12px] text-dim">Live discovery · {discovery.latency_ms} ms · protocol {discovery.protocol_version}. Selections are saved only when you save the connection.</p>
      {#if discovery.truncated}<p class="text-[13px] text-danger">Catalog truncated at 500 entries per category. Missing entries do not imply absence.</p>{/if}
      <h4 class="text-[13px] font-semibold">Tools ({discovery.tools.length})</h4>
      {#each discovery.tools as tool, index (`${tool.name}:${index}`)}
        <div class="rounded border border-line p-3">
          <label class="flex items-center gap-2 text-[13px]"><input type="checkbox" disabled={busy || changedURL} checked={identifierList(allowedToolText).includes(tool.name)} onchange={(e) => toggle("allowed_tools", tool.name, e.currentTarget.checked)} /><span class="font-mono">{tool.name}</span><span class="text-faint">{tool.annotations?.readOnlyHint ? "read-only hint (unverified)" : "no read-only hint"}</span></label>
          <p class="mt-1 whitespace-pre-wrap text-[12px] text-dim">{tool.description || "No description"}</p>
          <details class="mt-2 text-[12px]"><summary class="cursor-pointer text-dim">Input schema</summary><pre class="mt-2 max-h-64 overflow-auto whitespace-pre-wrap">{JSON.stringify(tool.inputSchema, null, 2)}</pre></details>
        </div>
      {/each}
      <h4 class="text-[13px] font-semibold">Resources ({discovery.resources.length})</h4>
      {#each discovery.resources as resource, index (`${resource.uri}:${index}`)}
        <label class="flex items-start gap-2 rounded border border-line p-3 text-[13px]"><input type="checkbox" disabled={busy || changedURL} checked={identifierList(allowedResourceText).includes(resource.uri)} onchange={(e) => toggle("allowed_resources", resource.uri, e.currentTarget.checked)} /><span class="min-w-0"><span class="block break-all font-mono">{resource.uri}</span><span class="block whitespace-pre-wrap text-[12px] text-dim">{resource.name} — {resource.description || "No description"}</span></span></label>
      {/each}
      <h4 class="text-[13px] font-semibold">Resource templates ({discovery.resource_templates.length})</h4>
      {#each discovery.resource_templates as template, index (`${template.uriTemplate}:${index}`)}
        <label class="flex items-start gap-2 rounded border border-line p-3 text-[13px]"><input type="checkbox" disabled={busy || changedURL} checked={identifierList(allowedResourceText).includes(template.uriTemplate)} onchange={(e) => toggle("allowed_resources", template.uriTemplate, e.currentTarget.checked)} /><span class="min-w-0"><span class="block break-all font-mono">{template.uriTemplate}</span><span class="block whitespace-pre-wrap text-[12px] text-dim">{template.name} — {template.description || "No description"}</span></span></label>
      {/each}
    {/if}
  </div>
{/if}
