<script>
  import Icon from "../lib/Icon.svelte";
  import { API_METHOD_COLORS, prettyOperationBody } from "../lib/api-operation.js";

  let {
    detail = null,
    detailBusy = false,
    tryOpen = $bindable(false),
    tryRequest = $bindable(),
    tryBusy = false,
    tryResult = null,
    onClose,
    onSend,
  } = $props();
</script>

<div
  class="fixed inset-0 z-40 flex justify-end bg-black/40"
  role="button"
  tabindex="-1"
  onclick={(event) => {
    if (event.target === event.currentTarget) onClose();
  }}
  onkeydown={(event) => event.key === "Escape" && onClose()}
>
  <div class="h-full w-full max-w-2xl overflow-y-auto bg-surface p-5 shadow-xl">
    {#if detailBusy}
      <div class="text-dim">Loading…</div>
    {:else if detail}
      {@const operation = detail.detail || {}}
      <div class="mb-3 flex items-start gap-2">
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <span class="font-mono text-[13px] font-medium {API_METHOD_COLORS[operation.method] || 'text-dim'}">
              {operation.method}
            </span>
            <span class="min-w-0 break-all font-mono text-[13.5px]">{operation.path}</span>
          </div>
          {#if operation.summary}<div class="mt-1 text-[13px] text-dim">{operation.summary}</div>{/if}
          <div class="mt-1 font-mono text-[11px] text-faint">{detail.operationId}</div>
        </div>
        <button class="icon-btn" onclick={onClose} aria-label="Close">
          <Icon name="x" size={16} />
        </button>
      </div>

      {#if operation.truncated}
        <div class="mb-3 rounded-md border border-warn bg-warn/10 px-3 py-2 text-[12px] text-warn">
          Some schema detail was omitted to keep this bounded; consult the source specification for
          the full definition.
        </div>
      {/if}

      {#if operation.description}
        <p class="mb-3 whitespace-pre-wrap text-[13px] text-dim">{operation.description}</p>
      {/if}

      {#if operation.request?.command}
        <h3 class="mb-1 text-[13px] font-medium">Example request</h3>
        <pre class="mb-3 overflow-x-auto rounded-md bg-surface-2 p-3 font-mono text-[11.5px]">{operation.request.command}</pre>
      {/if}

      <div class="mb-3 rounded-md border border-line">
        <button
          class="flex w-full items-center justify-between px-3 py-2 text-[13px] font-medium"
          onclick={() => (tryOpen = !tryOpen)}
        >
          <span>Try it</span>
          <Icon name={tryOpen ? "chevron-down" : "chevron-right"} size={14} />
        </button>

        {#if tryOpen}
          <div class="border-t border-line p-3">
            {#if tryRequest.pathParams.length}
              <div class="mb-1 text-[12px] font-medium text-dim">Path parameters</div>
              {#each tryRequest.pathParams as parameter (parameter.name)}
                <div class="mb-1 flex items-center gap-2">
                  <span class="w-40 shrink-0 truncate font-mono text-[12px]">
                    {parameter.name} <span class="text-warn">*</span>
                  </span>
                  <input class="input h-7 flex-1 text-[12px]" bind:value={parameter.value} placeholder="value" />
                </div>
              {/each}
            {/if}

            {#if tryRequest.query.length}
              <div class="mb-1 mt-2 text-[12px] font-medium text-dim">Query</div>
              {#each tryRequest.query as parameter (parameter.name)}
                <div class="mb-1 flex items-center gap-2">
                  <span class="w-40 shrink-0 truncate font-mono text-[12px]">
                    {parameter.name}{#if parameter.required}<span class="text-warn">*</span>{/if}
                  </span>
                  <input class="input h-7 flex-1 text-[12px]" bind:value={parameter.value} placeholder="value" />
                </div>
              {/each}
            {/if}

            <div class="mb-1 mt-2 flex items-center justify-between">
              <span class="text-[12px] font-medium text-dim">Headers</span>
              <button
                class="text-[12px] text-faint hover:text-dim"
                onclick={() => (tryRequest.headers = [...tryRequest.headers, { name: "", value: "" }])}
              >
                + add
              </button>
            </div>
            {#each tryRequest.headers as header, index (index)}
              <div class="mb-1 flex items-center gap-2">
                <input
                  class="input h-7 w-40 shrink-0 font-mono text-[12px]"
                  bind:value={header.name}
                  placeholder="Name"
                />
                <input
                  class="input h-7 flex-1 text-[12px]"
                  bind:value={header.value}
                  placeholder="value (empty removes a configured header)"
                />
              </div>
            {/each}

            {#if operation.request_body || operation.method === "GRPC"}
              <div class="mb-1 mt-2 text-[12px] font-medium text-dim">
                Body
                <span class="font-mono text-[11px] text-faint">
                  {operation.request_body?.content_type || "application/json"}
                </span>
              </div>
              <textarea class="input h-32 w-full font-mono text-[12px]" bind:value={tryRequest.body}></textarea>
            {/if}

            <div class="mt-2 flex items-center gap-2">
              <button class="btn btn-primary h-7 text-[12px]" disabled={tryBusy} onclick={onSend}>
                {tryBusy ? "Sending…" : "Send"}
              </button>
              <span class="text-[11px] text-faint">
                Sent by krabby using the service's configured credentials.
              </span>
            </div>

            {#if tryResult}
              <div class="mt-3 border-t border-line pt-2">
                <div class="flex items-center gap-3 text-[12.5px]">
                  <span class="font-mono font-medium {tryResult.ok ? 'text-ok' : 'text-err'}">
                    {tryResult.status_text}
                  </span>
                  {#if tryResult.duration_ms != null}<span class="text-faint">{tryResult.duration_ms} ms</span>{/if}
                  {#if tryResult.body_bytes}<span class="text-faint">{tryResult.body_bytes} B</span>{/if}
                  {#if tryResult.message_count}
                    <span class="text-faint">
                      {tryResult.message_count} message{tryResult.message_count > 1 ? "s" : ""}
                    </span>
                  {/if}
                </div>
                {#if tryResult.error}
                  <div class="mt-1 text-[12px] text-err">{tryResult.error}</div>
                {/if}
                {#if tryResult.truncated}
                  <div class="mt-1 text-[12px] text-warn">The response was truncated.</div>
                {/if}
                {#if tryResult.notes?.length}
                  {#each tryResult.notes as note (note)}
                    <div class="mt-1 text-[12px] text-faint">{note}</div>
                  {/each}
                {/if}
                {#if tryResult.body}
                  <pre class="mt-2 max-h-80 overflow-auto rounded-md bg-surface-2 p-3 font-mono text-[11.5px]">{prettyOperationBody(tryResult)}</pre>
                {/if}
              </div>
            {/if}
          </div>
        {/if}
      </div>

      {#if operation.parameters?.length}
        <h3 class="mb-1 text-[13px] font-medium">Parameters</h3>
        <table class="mb-3 w-full text-left text-[12px]">
          <thead class="text-faint">
            <tr><th class="py-1">Name</th><th>In</th><th>Type</th><th>Required</th></tr>
          </thead>
          <tbody>
            {#each operation.parameters as parameter (parameter.in + parameter.name)}
              <tr class="border-t border-line">
                <td class="py-1 font-mono">{parameter.name}</td>
                <td class="text-dim">{parameter.in}</td>
                <td class="text-dim">{parameter.type || "—"}</td>
                <td class="text-dim">{parameter.required ? "yes" : ""}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}

      {#if operation.request_body?.schema}
        <h3 class="mb-1 text-[13px] font-medium">
          Request body <span class="font-mono text-[11px] text-faint">{operation.request_body.content_type}</span>
        </h3>
        <pre class="mb-3 overflow-x-auto rounded-md bg-surface-2 p-3 font-mono text-[11.5px]">{JSON.stringify(operation.request_body.schema, null, 2)}</pre>
      {/if}

      {#if operation.responses?.length}
        <h3 class="mb-1 text-[13px] font-medium">Responses</h3>
        {#each operation.responses as response (response.status)}
          <div class="mb-2">
            <div class="text-[12.5px]">
              <span class="font-mono font-medium">{response.status}</span>
              {#if response.description}<span class="text-dim"> — {response.description}</span>{/if}
            </div>
            {#if response.schema}
              <pre class="mt-1 max-h-64 overflow-auto rounded-md bg-surface-2 p-3 font-mono text-[11.5px]">{JSON.stringify(response.schema, null, 2)}</pre>
            {/if}
          </div>
        {/each}
      {/if}

      {#if operation.notes?.length}
        <div class="mt-3 flex flex-col gap-1">
          {#each operation.notes as note (note)}
            <div class="text-[12px] text-faint">{note}</div>
          {/each}
        </div>
      {/if}
    {/if}
  </div>
</div>
