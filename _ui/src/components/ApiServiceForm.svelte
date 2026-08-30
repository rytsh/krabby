<script>
  let {
    form = $bindable(),
    editingName = "",
    kinds = [],
    busy = false,
    testResult = null,
    onSave,
    onTest,
    onCancel,
  } = $props();
</script>

<div class="card flex flex-col gap-3 p-4">
  <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Name (search scope)
      <input class="input" placeholder="e.g. billing" bind:value={form.name} disabled={!!editingName} />
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Kind
      <select class="input" bind:value={form.kind} disabled={!!editingName}>
        {#each kinds as kind (kind)}
          <option value={kind}>{kind === "grpc" ? "gRPC (server reflection)" : "OpenAPI / Swagger"}</option>
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
      <input
        class="input font-mono"
        placeholder="billing.v1.Invoices, billing.v1.Payments"
        bind:value={form.grpc_services}
      />
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
    <button class="btn btn-primary" onclick={onSave} disabled={busy}>
      {editingName ? "Save changes" : "Add service"}
    </button>
    <button class="btn" onclick={onTest} disabled={busy}>Test &amp; preview</button>
    <button class="btn" onclick={onCancel} disabled={busy}>Cancel</button>
  </div>
</div>
