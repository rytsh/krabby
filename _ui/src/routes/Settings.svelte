<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { successToast } from "../lib/toast.js";
  import { sidebarPathMode } from "../lib/paths.js";

  let settings = $state(null);
  let creds = $state([]);
  let credential = $state({ pattern: "", kind: "token", username: "", secret: "" });
  let credentialBusy = $state(false);
  let error = $state("");

  // Docs & RAG runtime config.
  let docsCfg = $state(null); // redacted config from the server
  let docsErr = $state("");
  let docsMsg = $state("");
  let saving = $state(false);
  let promptView = $state("custom");
  // Secret inputs are write-only; blank means "keep existing".
  let llmKey = $state("");
  let embedKey = $state("");
  let codeEmbedKey = $state("");
  let webhookSecret = $state("");
  let runtimeBusy = $state(false);
  let runtimeMsg = $state("");
  let runtimeErr = $state("");

  // Connection test state.
  let llmTest = $state(null); // { ok, latency_ms, model, error }
  let embedTest = $state(null); // { ok, dim, latency_ms, model, error }
  let codeEmbedTest = $state(null); // { ok, dim, latency_ms, model, error }
  let testingLLM = $state(false);
  let testingEmbed = $state(false);
  let testingCodeEmbed = $state(false);

  function logTestFailure(name, result) {
    if (result && !result.ok) {
      console.error(`[krabby] ${name} test failed`, result);
    }
  }

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

  async function saveCredential() {
    credentialBusy = true;
    try {
      await api.setCredential(credential);
      creds = await api.credentials();
      credential = { pattern: "", kind: "token", username: "", secret: "" };
      successToast("Credential saved");
    } catch (e) {
      error = e.message;
    } finally {
      credentialBusy = false;
    }
  }

  async function removeCredential(pattern) {
    if (!confirm(`Delete credential for ${pattern}?`)) return;
    try {
      await api.deleteCredential(pattern);
      creds = await api.credentials();
    } catch (e) {
      error = e.message;
    }
  }

  // buildPatch produces the request body from the form. Blank secret fields
  // mean "keep the stored value" (the server merges them).
  function buildPatch() {
    const patch = { ...docsCfg };
    delete patch.llm_api_key_set;
    delete patch.embed_api_key_set;
    delete patch.code_embed_api_key_set;
    delete patch.docs_default_prompt;
    delete patch.updated_at;
    patch.llm_api_key = llmKey;
    patch.embed_api_key = embedKey;
    patch.code_embed_api_key = codeEmbedKey;
    return patch;
  }

  async function saveDocs() {
    saving = true;
    docsErr = "";
    docsMsg = "";
    try {
      docsCfg = await api.setDocsConfig(buildPatch());
      llmKey = embedKey = codeEmbedKey = "";
      docsMsg = "Saved. Existing repositories queued for reindex.";
      successToast("Saved");
    } catch (e) {
      docsErr = e.message;
    } finally {
      saving = false;
    }
  }

  // Git polling and webhook verification are persisted alongside the docs
  // settings, but saved independently so changing them does not require
  // editing the larger Docs & RAG form. Durations are Go nanoseconds in the
  // REST representation; the select keeps those values explicit.
  async function saveRuntime(clearWebhook = false) {
    if (!docsCfg) return;
    runtimeBusy = true;
    runtimeMsg = "";
    runtimeErr = "";
    try {
      const patch = {
        git_poll_interval: Number(docsCfg.git_poll_interval),
        task_concurrency: Number(docsCfg.task_concurrency),
      };
      if (clearWebhook) patch.webhook_secret = "";
      else if (webhookSecret) patch.webhook_secret = webhookSecret;
      docsCfg = await api.setDocsConfig(patch);
      webhookSecret = "";
      runtimeMsg = clearWebhook ? "Webhook verification disabled." : "Runtime settings saved.";
      successToast("Saved");
    } catch (e) {
      runtimeErr = e.message;
    } finally {
      runtimeBusy = false;
    }
  }

  async function testLLM() {
    testingLLM = true;
    llmTest = null;
    try {
      llmTest = await api.testLLM(buildPatch());
      logTestFailure("LLM", llmTest);
    } catch (e) {
      llmTest = { ok: false, error: e.message };
      console.error("[krabby] LLM test request failed", e);
    } finally {
      testingLLM = false;
    }
  }

  async function testEmbedder() {
    testingEmbed = true;
    embedTest = null;
    try {
      embedTest = await api.testEmbedder(buildPatch());
      logTestFailure("embedder", embedTest);
    } catch (e) {
      embedTest = { ok: false, error: e.message };
      console.error("[krabby] Embedder test request failed", e);
    } finally {
      testingEmbed = false;
    }
  }

  async function testCodeEmbedder() {
    testingCodeEmbed = true;
    codeEmbedTest = null;
    try {
      codeEmbedTest = await api.testCodeEmbedder(buildPatch());
      logTestFailure("code embedder", codeEmbedTest);
    } catch (e) {
      codeEmbedTest = { ok: false, error: e.message };
      console.error("[krabby] Code embedder test request failed", e);
    } finally {
      testingCodeEmbed = false;
    }
  }

  function useDefaultPrompt() {
    docsCfg.docs_prompt = docsCfg.docs_default_prompt;
    promptView = "custom";
  }

  // MCP API key runtime management. The key is write-only; only the set/unset
  // state is shown.
  let mcpKeyInput = $state("");
  let mcpMsg = $state("");
  let mcpErr = $state("");
  let mcpBusy = $state(false);

  async function mcpAction(fn, okMsg) {
    mcpBusy = true;
    mcpErr = "";
    mcpMsg = "";
    try {
      const res = await fn();
      if (settings) settings = { ...settings, mcp: { ...settings.mcp, api_key_set: res.api_key_set } };
      mcpKeyInput = "";
      mcpMsg = okMsg;
    } catch (e) {
      mcpErr = e.message;
    } finally {
      mcpBusy = false;
    }
  }

  const saveMcpKey = () =>
    mcpAction(() => api.setMcpKey(mcpKeyInput.trim()), "API key set. Clients must now send it.");
  const disableMcpKey = () =>
    mcpAction(() => api.setMcpKey(""), "Authentication disabled; the MCP endpoint is open.");
  const resetMcpKey = () =>
    mcpAction(() => api.clearMcpKey(), "Override removed; the config/env value applies again.");

  onMount(load);

  // Rows rendered as [label, value] with an optional boolean "set" style.
  function rows(s) {
    return [
      ["Version", s.version],
      ["Commit", s.commit],
      ["Build date", s.build_date],
      ["Log level", s.log_level],
      ["Data dir", s.data_dir],
      ["Listen", `${s.server.host || "0.0.0.0"}:${s.server.port}`],
      ["MCP path", s.mcp.path],
      ["MCP profiles", "standard (default), full (X-Krabby-Tool-Profile: full)"],
      ["MCP API key", s.mcp.api_key_set ? "set" : "not set", s.mcp.api_key_set],
      ["Graphify bin", s.graphify.bin],
      ["Graphify python", s.graphify.python || "auto (shebang)"],
      ["Build timeout", s.graphify.build_timeout],
    ];
  }
</script>

<p class="text-dim">Read-only view of the running configuration. Secrets are never shown.</p>

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

  <h2 class="mb-1 mt-8 text-[15px] font-semibold">MCP access</h2>
  <p class="text-dim">
    Protect the MCP endpoint with an API key. Changes apply immediately — no restart needed. Clients
    send the key in the <code class="font-mono text-[12px]">X-Api-Key</code> header. The endpoint uses the
    standard tool profile by default; send <code class="font-mono text-[12px]">X-Krabby-Tool-Profile: full</code>
    to expose credential, lease, and docs/RAG administration tools.
  </p>

  <div class="card mt-3 p-4">
    <div class="mb-3 flex items-center gap-2 text-[13px]">
      <span class="text-dim">Status</span>
      <span class={settings.mcp.api_key_set ? "text-ok" : "text-faint"}>
        {settings.mcp.api_key_set ? "protected (API key required)" : "open (no API key)"}
      </span>
    </div>

    <div class="flex gap-2">
      <input
        class="input flex-1"
        type="password"
        placeholder="new API key"
        bind:value={mcpKeyInput}
        onkeydown={(e) => e.key === "Enter" && mcpKeyInput.trim() && saveMcpKey()}
      />
      <button class="btn btn-primary" disabled={mcpBusy || !mcpKeyInput.trim()} onclick={saveMcpKey}>
        Set key
      </button>
      <button
        class="btn btn-danger"
        disabled={mcpBusy || !settings.mcp.api_key_set}
        onclick={disableMcpKey}
        title="Remove the API key requirement entirely"
      >
        Disable auth
      </button>
      <button
        class="btn"
        disabled={mcpBusy}
        onclick={resetMcpKey}
        title="Drop the runtime override and fall back to the config/env value"
      >
        Reset to config
      </button>
    </div>

    {#if mcpMsg}
      <p class="mb-0 mt-3 text-[13px] text-ok">{mcpMsg}</p>
    {/if}
  </div>

  <h2 class="mb-3 mt-8 text-[15px] font-semibold">Git credentials</h2>
  <p class="mb-3 text-dim">
    Host or host/path credentials for private git repositories and custom web pages. The most
    specific pattern wins; secrets are write-only.
  </p>
  <div class="card mb-3 grid grid-cols-1 gap-2 p-3 sm:grid-cols-[1fr_120px_180px_1fr_auto]">
    <input class="input" placeholder="git.example.com/group" bind:value={credential.pattern} />
    <select class="input" bind:value={credential.kind}>
      <option value="token">Token</option>
      <option value="bearer">Bearer (web)</option>
      <option value="ssh">SSH key</option>
    </select>
    <input
      class="input"
      placeholder={credential.kind === "token" ? "username (optional)" : "not used"}
      bind:value={credential.username}
      disabled={credential.kind !== "token"}
    />
    {#if credential.kind !== "ssh"}
      <input class="input" type="password" placeholder="token / password" bind:value={credential.secret} />
    {:else}
      <textarea class="input" placeholder="private key PEM" bind:value={credential.secret} rows="2"></textarea>
    {/if}
    <button
      class="btn btn-primary"
      onclick={saveCredential}
      disabled={credentialBusy || !credential.pattern.trim() || !credential.secret}
    >Save</button>
  </div>
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
            <th class="border-b border-line px-4 py-2"></th>
          </tr>
        </thead>
        <tbody>
          {#each creds as c}
            <tr class="hover:bg-surface-2">
              <td class="border-b border-line px-4 py-2.5 font-mono text-[13px]">{c.pattern}</td>
              <td class="border-b border-line px-4 py-2.5 text-[13px] text-faint">{c.kind}</td>
              <td class="border-b border-line px-4 py-2.5 text-[13px] text-faint">{c.username || "—"}</td>
              <td class="border-b border-line px-4 py-2.5 text-right">
                <button class="btn btn-sm btn-danger" onclick={() => removeCredential(c.pattern)}>Delete</button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
{:else if !error}
  <div class="mt-4 text-dim">Loading…</div>
{/if}

<h2 class="mb-1 mt-10 text-[15px] font-semibold">Appearance</h2>
<p class="text-dim">Display preferences, stored in this browser only.</p>

<div class="card mt-3 p-4">
  <div class="flex flex-wrap items-center justify-between gap-3 text-[13px]">
    <div class="flex min-w-0 flex-col gap-0.5">
      <span>Sidebar repository paths</span>
      <span class="text-[12px] text-faint">
        Repos are tracked by their full path (host/group/…/name). Smart hides the parts every group
        shares and keeps one parent segment for context; full always shows the complete path.
      </span>
    </div>
    <div class="flex shrink-0 gap-1" role="tablist" aria-label="Sidebar path display">
      <button
        type="button"
        class="btn btn-sm"
        class:btn-primary={$sidebarPathMode === "smart"}
        role="tab"
        aria-selected={$sidebarPathMode === "smart"}
        onclick={() => sidebarPathMode.set("smart")}>Smart</button
      >
      <button
        type="button"
        class="btn btn-sm"
        class:btn-primary={$sidebarPathMode === "full"}
        role="tab"
        aria-selected={$sidebarPathMode === "full"}
        onclick={() => sidebarPathMode.set("full")}>Full path</button
      >
    </div>
  </div>
</div>

<h2 class="mb-1 mt-10 text-[15px] font-semibold">Runtime</h2>
<p class="text-dim">
  Repository polling, background task concurrency and webhook security. Changes apply without a restart.
</p>

{#if docsCfg}
  <div class="card mt-3 p-4">
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Repository poll interval
        <select
          class="input"
          value={String(docsCfg.git_poll_interval)}
          onchange={(e) => (docsCfg.git_poll_interval = Number(e.currentTarget.value))}
        >
          <option value="-1">disabled</option>
          <option value="900000000000">every 15 minutes</option>
          <option value="3600000000000">every hour</option>
          <option value="21600000000000">every 6 hours</option>
          <option value="86400000000000">daily</option>
        </select>
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Concurrent tasks
        <input class="input" type="number" min="1" max="64" bind:value={docsCfg.task_concurrency} />
        <span class="text-[12px] text-faint">
          How many background tasks (refresh, generate, web sync, reindex) run at once. Lower to protect
          git/graphify/LLM/embedder backends; raise to process more repositories in parallel.
        </span>
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Git webhook secret {docsCfg.webhook_secret_set ? "(set)" : "(not set)"}
        <input
          class="input"
          type="password"
          bind:value={webhookSecret}
          placeholder="leave blank to keep existing"
        />
      </label>
    </div>
    <div class="mt-3 flex items-center gap-2">
      <button class="btn btn-primary" onclick={() => saveRuntime(false)} disabled={runtimeBusy}>Save runtime settings</button>
      <button
        class="btn btn-danger"
        onclick={() => saveRuntime(true)}
        disabled={runtimeBusy || !docsCfg.webhook_secret_set}
      >Disable webhook verification</button>
      {#if runtimeMsg}<span class="text-[12px] text-ok">{runtimeMsg}</span>{/if}
      {#if runtimeErr}<span class="text-[12px] text-err">{runtimeErr}</span>{/if}
    </div>
  </div>
{/if}

<h2 class="mb-1 mt-10 text-[15px] font-semibold">Docs &amp; RAG</h2>
<p class="text-dim">
  Generate markdown docs per repo, embed them into a vector store, and expose retrieval over
  MCP/REST. Changes rebuild the clients live. API keys are write-only — leave blank to keep the
  stored value.
</p>

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
        <button class="btn btn-sm" onclick={testLLM} disabled={testingLLM}>
          {testingLLM ? "Testing…" : "Test LLM"}
        </button>
      </span>
    </div>
    <label class="mb-3 flex items-center gap-2 text-[13px]">
      <input type="checkbox" bind:checked={docsCfg.docs_enabled} />
      Generate markdown docs on refresh
    </label>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
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
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Max summary groups
        <input class="input" type="number" bind:value={docsCfg.docs_max_groups} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Summary model (fast; blank = main model)
        <input class="input" bind:value={docsCfg.docs_summary_model} placeholder="e.g. google-ai/gemini-2.5-flash" />
      </label>
    </div>

    <div class="mt-3 flex flex-col gap-1 text-[13px] text-dim">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <span>Doc generation prompt (system)</span>
        <div class="flex gap-1" role="tablist" aria-label="Prompt view">
          <button
            type="button"
            class="btn btn-sm"
            class:btn-primary={promptView === "custom"}
            role="tab"
            aria-selected={promptView === "custom"}
            onclick={() => (promptView = "custom")}>Custom</button
          >
          <button
            type="button"
            class="btn btn-sm"
            class:btn-primary={promptView === "default"}
            role="tab"
            aria-selected={promptView === "default"}
            onclick={() => (promptView = "default")}>Default (read-only)</button
          >
        </div>
      </div>
      {#if promptView === "custom"}
        <textarea
          class="input font-mono text-[12px]"
          rows="12"
          bind:value={docsCfg.docs_prompt}
          placeholder="Leave blank to use the built-in default prompt."
        ></textarea>
      {:else}
        <textarea
          class="input bg-surface-2 font-mono text-[12px]"
          rows="12"
          readonly
          value={docsCfg.docs_default_prompt}
        ></textarea>
        <div class="mt-1 flex items-center justify-between gap-3">
          <span class="text-[12px] text-faint">Built into this krabby version. Select and copy any part you need.</span>
          <button type="button" class="btn btn-sm shrink-0" onclick={useDefaultPrompt}>Use as custom</button>
        </div>
      {/if}
      <span class="text-[12px] text-faint">
        Sent as the system message for each file. The file content and its graph neighborhood are
        appended as the user message. Blank = built-in default.
      </span>
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
        <button class="btn btn-sm" onclick={testEmbedder} disabled={testingEmbed}>
          {testingEmbed ? "Testing…" : "Test embedder"}
        </button>
      </span>
    </div>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
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

    <!-- Source-code embeddings -->
    <div class="mb-2 mt-6 flex items-center justify-between">
      <span class="text-[13px] font-semibold text-dim">Code embeddings</span>
      <span class="flex items-center gap-2">
        {#if codeEmbedTest}
          {#if codeEmbedTest.ok}
            <span class="text-[12px] text-ok">
              ✓ ok · {codeEmbedTest.model || "?"} · dim {codeEmbedTest.dim || "?"} · {codeEmbedTest.latency_ms}ms
            </span>
          {:else}
            <span class="max-w-[24rem] truncate text-[12px] text-err" title={codeEmbedTest.error}>✗ {codeEmbedTest.error}</span>
          {/if}
        {/if}
        <button class="btn btn-sm" onclick={testCodeEmbedder} disabled={testingCodeEmbed}>
          {testingCodeEmbed ? "Testing…" : "Test code embedder"}
        </button>
      </span>
    </div>
    <label class="mb-3 flex items-center gap-2 text-[13px]">
      <input type="checkbox" bind:checked={docsCfg.code_rag_enabled} />
      Enable semantic code search
    </label>
    <p class="mb-3 text-[12px] text-faint">
      Normal code search always uses the local bw full-text index. Enable this option for vector-based
      semantic search; leave the code embedder URL blank to reuse the docs embedder.
    </p>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code embedder base URL
        <input class="input" bind:value={docsCfg.code_embed_base_url} placeholder="https://api.mistral.ai/v1" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code embedder model
        <input class="input" bind:value={docsCfg.code_embed_model} placeholder="codestral-embed-2505" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code embedder API key {docsCfg.code_embed_api_key_set ? "(set)" : "(not set)"}
        <input class="input" type="password" bind:value={codeEmbedKey} placeholder="leave blank to keep" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code embedding dim (0 = infer)
        <input class="input" type="number" bind:value={docsCfg.code_embed_dim} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code chunk size (chars)
        <input class="input" type="number" bind:value={docsCfg.code_rag_chunk_size} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code chunk overlap (chars)
        <input class="input" type="number" bind:value={docsCfg.code_rag_chunk_overlap} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code snippets returned (top_k)
        <input class="input" type="number" bind:value={docsCfg.code_rag_top_k} />
      </label>
    </div>

    <!-- Retrieval -->
    <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Retrieval</div>
    <label class="mb-3 flex items-center gap-2 text-[13px]">
      <input type="checkbox" bind:checked={docsCfg.rag_enabled} />
      Enable RAG indexing &amp; retrieval
    </label>
    <p class="mb-3 text-[12px] text-faint">Vectors are stored locally in embedded bw indexes.</p>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
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

    <div class="mt-6">
      <button class="btn btn-primary" onclick={saveDocs} disabled={saving}>
        {saving ? "Saving…" : "Save & rebuild"}
      </button>
    </div>
  </div>
{:else if !docsErr}
  <div class="mt-4 text-dim">Loading…</div>
{/if}
