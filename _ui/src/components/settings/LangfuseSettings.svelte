<script>
  let {
    draft = $bindable(),
    secretKey = $bindable(),
    testResult = null,
    testing = false,
    busy = false,
    message = "",
    error = "",
    onTest,
    onSave,
  } = $props();
</script>

<h2 class="mb-1 mt-10 text-[15px] font-semibold">LLM observability</h2>
<p class="mb-3 text-[13px] text-dim">
  Export every model call to Langfuse as a trace: model, latency, time to first token, token
  usage and cost. Traces are sent over OTLP/HTTP on a tracer provider separate from the
  <code>telemetry</code> collector, because Langfuse does not accept gRPC. Saving here rebuilds
  the clients but does not reindex anything.
</p>

<div class="card p-4">
  <div class="mb-3 flex items-center justify-between">
    <label class="flex items-center gap-2 text-[13px]">
      <input type="checkbox" bind:checked={draft.langfuse_enabled} />
      Enable Langfuse export
    </label>
    <span class="flex items-center gap-2">
      {#if testResult}
        {#if testResult.ok}
          <span class="text-[12px] text-ok">
            ✓ ok{testResult.model ? ` · project ${testResult.model}` : ""} · {testResult.latency_ms}ms
          </span>
        {:else}
          <span class="max-w-[24rem] truncate text-[12px] text-err" title={testResult.error}>✗ {testResult.error}</span>
        {/if}
      {/if}
      <button class="btn btn-sm" onclick={onTest} disabled={testing}>
        {testing ? "Testing…" : "Test connection"}
      </button>
    </span>
  </div>

  <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Host
      <input class="input" bind:value={draft.langfuse_host} placeholder="https://cloud.langfuse.com" />
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Environment
      <input class="input" bind:value={draft.langfuse_environment} placeholder="production" />
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Public key
      <input class="input" bind:value={draft.langfuse_public_key} placeholder="pk-lf-…" />
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Secret key {draft.langfuse_secret_key_set ? "(set)" : "(not set)"}
      <input class="input" type="password" bind:value={secretKey} placeholder="leave blank to keep" />
    </label>
  </div>
  <p class="mt-2 text-[12px] text-faint">
    For the EU region use <code>https://cloud.langfuse.com</code>; US, Japan and HIPAA have their
    own hosts. A self-hosted instance needs v3.22.0 or newer for the OTLP endpoint.
  </p>

  <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">What gets traced</div>
  <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
    <label class="flex items-start gap-2 text-[13px]">
      <input type="checkbox" class="mt-1" bind:checked={draft.langfuse_trace_docs} />
      <span>
        Documentation LLM calls
        <span class="block text-[12px] text-faint">
          One trace per docs build, one generation per summary group plus the synthesis.
        </span>
      </span>
    </label>
    <label class="flex items-start gap-2 text-[13px]">
      <input type="checkbox" class="mt-1" bind:checked={draft.langfuse_trace_embed} />
      <span>
        Embedding calls
        <span class="block text-[12px] text-faint">
          One observation per Embed call, not per batch — a large index would otherwise emit
          thousands.
        </span>
      </span>
    </label>
    <label class="flex items-start gap-2 text-[13px]">
      <input type="checkbox" class="mt-1" bind:checked={draft.langfuse_trace_mcp} />
      <span>
        MCP tool calls
        <span class="block text-[12px] text-faint">
          Shows what a connected agent actually asked for. No model or token data.
        </span>
      </span>
    </label>
    <label class="flex items-start gap-2 text-[13px]">
      <input type="checkbox" class="mt-1" bind:checked={draft.langfuse_trace_http} />
      <span>
        REST API requests
        <span class="block text-[12px] text-faint">
          Wraps each <code>/api/v1</code> call in a trace, so a search made from this UI shows the
          embedding it caused underneath it instead of as a standalone observation. Only the API
          is covered — health checks, the UI's own assets and the MCP endpoint are not.
          Off by default: the UI polls, so most of what this adds is requests that did no model
          work at all.
        </span>
      </span>
    </label>
  </div>

  <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Prompt &amp; completion capture</div>
  <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Capture mode
      <select class="input" bind:value={draft.langfuse_capture}>
        <option value="full">full — send prompts and replies whole</option>
        <option value="truncated">truncated — clip to 8 KiB</option>
        <option value="off">off — metadata only</option>
      </select>
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Max bytes per value (0 = no limit)
      <input class="input" type="number" min="0" bind:value={draft.langfuse_max_content_bytes} />
    </label>
  </div>

  {#if draft.langfuse_enabled && draft.langfuse_capture === "full"}
    <div class="mt-3 rounded border border-warn/40 bg-warn/10 p-3 text-[12px]">
      <div class="mb-1 font-semibold text-warn">What "full" capture means</div>
      <ul class="list-disc space-y-1 pl-4 text-dim">
        <li>
          <b>Your source code leaves this process.</b> Summary prompts embed the files being
          documented, so private repository contents are sent to
          {draft.langfuse_host || "the configured Langfuse instance"} verbatim. On Langfuse Cloud
          that is a third party.
        </li>
        <li>
          <b>The payload is large.</b> A synthesis prompt reaches 256 KiB and a summary prompt
          96 KiB; a forty-group build exports a few megabytes. Krabby caps the export batch at 8
          spans and the queue at 256 to keep that off the memory budget, so a very busy build can
          drop spans rather than grow.
        </li>
        <li>
          <b>Removing the byte cap is not free.</b> Setting max bytes to 0 lets a single
          attribute exceed what a hosted Langfuse will accept, and the whole batch is rejected.
          Only do it against a self-hosted instance with a matching body limit.
        </li>
      </ul>
      <div class="mt-2 text-dim">
        Use <b>truncated</b> to keep prompts debuggable at 8 KiB, or <b>off</b> to export only
        model, latency, tokens and cost.
      </div>
    </div>
  {/if}

  {#if error}<div class="mt-3 text-[13px] text-err">{error}</div>{/if}
  {#if message}<div class="mt-3 text-[13px] text-ok">{message}</div>{/if}

  <div class="mt-6">
    <button class="btn btn-primary" onclick={onSave} disabled={busy}>
      {busy ? "Saving…" : "Save observability settings"}
    </button>
  </div>
</div>
