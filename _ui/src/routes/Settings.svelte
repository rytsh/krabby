<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";

  let settings = null;
  let creds = [];
  let error = "";

  // Docs & RAG runtime config.
  let docsCfg = null; // redacted config from the server
  let docsErr = "";
  let docsMsg = "";
  let saving = false;
  // Secret inputs are write-only; blank means "keep existing".
  let llmKey = "";
  let embedKey = "";
  let qdrantKey = "";

  // Connection test state.
  let llmTest = null; // { ok, latency_ms, model, error }
  let embedTest = null; // { ok, dim, latency_ms, model, error }
  let testingLLM = false;
  let testingEmbed = false;

  async function load() {
    try {
      settings = await api.settings();
      try {
        creds = await api.credentials();
      } catch {
        creds = [];
      }
    } catch (e) {
      error = e.message;
    }

    try {
      docsCfg = await api.docsConfig();
    } catch (e) {
      docsErr = e.message;
    }
  }

  // buildPatch produces the request body from the form. Blank secret fields
  // mean "keep the stored value" (the server merges them).
  function buildPatch() {
    const patch = { ...docsCfg };
    delete patch.llm_api_key_set;
    delete patch.embed_api_key_set;
    delete patch.qdrant_api_key_set;
    delete patch.updated_at;
    patch.llm_api_key = llmKey;
    patch.embed_api_key = embedKey;
    patch.qdrant_api_key = qdrantKey;
    return patch;
  }

  async function saveDocs() {
    saving = true;
    docsErr = "";
    docsMsg = "";
    try {
      docsCfg = await api.setDocsConfig(buildPatch());
      llmKey = embedKey = qdrantKey = "";
      docsMsg = "Saved. Clients rebuilt.";
    } catch (e) {
      docsErr = e.message;
    } finally {
      saving = false;
    }
  }

  async function testLLM() {
    testingLLM = true;
    llmTest = null;
    try {
      llmTest = await api.testLLM(buildPatch());
    } catch (e) {
      llmTest = { ok: false, error: e.message };
    } finally {
      testingLLM = false;
    }
  }

  async function testEmbedder() {
    testingEmbed = true;
    embedTest = null;
    try {
      embedTest = await api.testEmbedder(buildPatch());
    } catch (e) {
      embedTest = { ok: false, error: e.message };
    } finally {
      testingEmbed = false;
    }
  }

  onMount(load);

  // Rows rendered as [label, value] with an optional boolean "set" style.
  function rows(s) {
    return [
      ["Version", s.version],
      ["Log level", s.log_level],
      ["Data dir", s.data_dir],
      ["Listen", `${s.server.host || "0.0.0.0"}:${s.server.port}`],
      ["MCP path", s.mcp.path],
      ["MCP API key", s.mcp.api_key_set ? "set" : "not set", s.mcp.api_key_set],
      ["Git SSH key", s.git.ssh_key_path || "—"],
      ["Poll interval", s.git.poll_interval],
      ["Graphify bin", s.graphify.bin],
      ["Graphify python", s.graphify.python || "auto (shebang)"],
      ["Build timeout", s.graphify.build_timeout],
      ["Serve idle timeout", s.graphify.serve_idle_timeout],
      ["Webhook secret", s.webhook.github_secret_set ? "set" : "not set", s.webhook.github_secret_set],
    ];
  }
</script>

<p class="text-dim">Read-only view of the running configuration. Secrets are never shown.</p>

{#if error}
  <div class="err-box mt-4">{error}</div>
{/if}

{#if settings}
  <div class="card mt-4 overflow-hidden">
    <table class="w-full border-collapse">
      <tbody>
        {#each rows(settings) as [label, value, isBool]}
          <tr class="hover:bg-surface-2">
            <td class="w-56 border-b border-line px-4 py-2.5 text-[13px] text-dim">{label}</td>
            <td class="border-b border-line px-4 py-2.5 font-mono text-[13px]">
              {#if isBool !== undefined}
                <span class={value === "set" ? "text-ok" : "text-faint"}>{value}</span>
              {:else}
                {value}
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>

  <h2 class="mb-3 mt-8 text-[15px] font-semibold">Git credentials</h2>
  <div class="card overflow-hidden">
    {#if creds.length === 0}
      <div class="p-6 text-center text-dim">No credentials stored.</div>
    {:else}
      <table class="w-full border-collapse">
        <thead>
          <tr class="text-[13px] text-dim">
            <th class="border-b border-line px-4 py-2 text-left font-medium">Pattern</th>
            <th class="border-b border-line px-4 py-2 text-left font-medium">Kind</th>
            <th class="border-b border-line px-4 py-2 text-left font-medium">Username</th>
          </tr>
        </thead>
        <tbody>
          {#each creds as c}
            <tr class="hover:bg-surface-2">
              <td class="border-b border-line px-4 py-2.5 font-mono text-[13px]">{c.pattern}</td>
              <td class="border-b border-line px-4 py-2.5 text-[13px] text-faint">{c.kind}</td>
              <td class="border-b border-line px-4 py-2.5 text-[13px] text-faint">{c.username || "—"}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
{:else if !error}
  <div class="mt-4 text-dim">Loading…</div>
{/if}

<h2 class="mb-1 mt-10 text-[15px] font-semibold">Docs &amp; RAG</h2>
<p class="text-dim">
  Generate markdown docs per repo, embed them into a vector store, and expose retrieval over
  MCP/REST. Changes rebuild the clients live. API keys are write-only — leave blank to keep the
  stored value.
</p>

{#if docsErr}
  <div class="err-box mt-4">{docsErr}</div>
{/if}
{#if docsMsg}
  <div class="mt-4 rounded-md border border-ok bg-ok/10 px-3 py-2.5 text-[13px] text-ok">{docsMsg}</div>
{/if}

{#if docsCfg}
  <div class="card mt-4 p-4">
    <!-- Documentation generation -->
    <div class="mb-2 flex items-center justify-between">
      <span class="text-[13px] font-semibold text-dim">Documentation generation (LLM)</span>
      <span class="flex items-center gap-2">
        {#if llmTest}
          {#if llmTest.ok}
            <span class="text-[12px] text-ok">✓ ok · {llmTest.model || "?"} · {llmTest.latency_ms}ms</span>
          {:else}
            <span class="max-w-[24rem] truncate text-[12px] text-err" title={llmTest.error}>✗ {llmTest.error}</span>
          {/if}
        {/if}
        <button class="btn btn-sm" on:click={testLLM} disabled={testingLLM}>
          {testingLLM ? "Testing…" : "Test LLM"}
        </button>
      </span>
    </div>
    <label class="mb-3 flex items-center gap-2 text-[13px]">
      <input type="checkbox" bind:checked={docsCfg.docs_enabled} />
      Generate markdown docs on refresh
    </label>
    <div class="grid grid-cols-2 gap-3">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        LLM base URL
        <input class="input" bind:value={docsCfg.llm_base_url} placeholder="https://api.openai.com/v1" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        LLM model
        <input class="input" bind:value={docsCfg.llm_model} placeholder="gpt-4o-mini" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        LLM API key {docsCfg.llm_api_key_set ? "(set)" : "(not set)"}
        <input class="input" type="password" bind:value={llmKey} placeholder="leave blank to keep" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Doc concurrency
        <input class="input" type="number" bind:value={docsCfg.docs_concurrency} />
      </label>
    </div>

    <!-- Embeddings -->
    <div class="mb-2 mt-6 flex items-center justify-between">
      <span class="text-[13px] font-semibold text-dim">Embeddings</span>
      <span class="flex items-center gap-2">
        {#if embedTest}
          {#if embedTest.ok}
            <span class="text-[12px] text-ok">
              ✓ ok · {embedTest.model || "?"} · dim {embedTest.dim || "?"} · {embedTest.latency_ms}ms
            </span>
          {:else}
            <span class="max-w-[24rem] truncate text-[12px] text-err" title={embedTest.error}>✗ {embedTest.error}</span>
          {/if}
        {/if}
        <button class="btn btn-sm" on:click={testEmbedder} disabled={testingEmbed}>
          {testingEmbed ? "Testing…" : "Test embedder"}
        </button>
      </span>
    </div>
    <div class="grid grid-cols-2 gap-3">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Embedder base URL
        <input class="input" bind:value={docsCfg.embed_base_url} placeholder="http://localhost:11434/v1" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Embedder model
        <input class="input" bind:value={docsCfg.embed_model} placeholder="nomic-embed-text" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Embedder API key {docsCfg.embed_api_key_set ? "(set)" : "(not set)"}
        <input class="input" type="password" bind:value={embedKey} placeholder="leave blank to keep" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Embedding dim (0 = infer)
        <input class="input" type="number" bind:value={docsCfg.embed_dim} />
      </label>
    </div>

    <!-- Retrieval + store -->
    <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Retrieval &amp; vector store</div>
    <label class="mb-3 flex items-center gap-2 text-[13px]">
      <input type="checkbox" bind:checked={docsCfg.rag_enabled} />
      Enable RAG indexing &amp; retrieval
    </label>
    <div class="grid grid-cols-2 gap-3">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Vector store
        <select class="input" bind:value={docsCfg.store_kind}>
          <option value="embedded">embedded (file-backed)</option>
          <option value="qdrant">qdrant</option>
        </select>
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Docs returned (top_docs)
        <input class="input" type="number" bind:value={docsCfg.rag_top_docs} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Chunk size (chars)
        <input class="input" type="number" bind:value={docsCfg.rag_chunk_size} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Chunk overlap (chars)
        <input class="input" type="number" bind:value={docsCfg.rag_chunk_overlap} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Chunk matches (top_k)
        <input class="input" type="number" bind:value={docsCfg.rag_top_k} />
      </label>
    </div>

    {#if docsCfg.store_kind === "qdrant"}
      <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Qdrant</div>
      <div class="grid grid-cols-2 gap-3">
        <label class="flex flex-col gap-1 text-[13px] text-dim">
          URL
          <input class="input" bind:value={docsCfg.qdrant_url} placeholder="http://localhost:6333" />
        </label>
        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Collection
          <input class="input" bind:value={docsCfg.qdrant_collection} placeholder="krabby" />
        </label>
        <label class="flex flex-col gap-1 text-[13px] text-dim">
          API key {docsCfg.qdrant_api_key_set ? "(set)" : "(not set)"}
          <input class="input" type="password" bind:value={qdrantKey} placeholder="leave blank to keep" />
        </label>
      </div>
    {/if}

    <div class="mt-6">
      <button class="btn btn-primary" on:click={saveDocs} disabled={saving}>
        {saving ? "Saving…" : "Save & rebuild"}
      </button>
    </div>
  </div>
{:else if !docsErr}
  <div class="mt-4 text-dim">Loading…</div>
{/if}
