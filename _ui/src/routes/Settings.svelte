<script>
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { successToast } from "../lib/toast.js";
  import { sidebarPathMode } from "../lib/paths.js";
  import RuntimeSettings from "../components/settings/RuntimeSettings.svelte";
  import LangfuseSettings from "../components/settings/LangfuseSettings.svelte";
  import {
    buildDocsPayload,
    buildLangfusePayload,
    buildRuntimePayload,
    createDocsDraft,
    createLangfuseDraft,
    createRuntimeDraft,
    normalizeSettingsSnapshot,
  } from "../lib/settings-config.js";

  let settings = $state(null);
  let creds = $state([]);
  let credential = $state({ pattern: "", kind: "token", username: "", secret: "" });
  let credentialBusy = $state(false);
  let error = $state("");

  // The server snapshot is authoritative and immutable. Each form edits its
  // own draft so another section's save cannot submit or replace those edits.
  let serverCfg = $state.raw(null);
  let runtimeDraft = $state(null);
  let docsDraft = $state(null);
  let langfuseDraft = $state(null);
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
  // Existing namespaces, used to suggest schedule targets. "*" (all) and the
  // default bucket are always available regardless of what is stored.
  let namespaceOptions = $state([]);

  // Connection test state.
  let llmTest = $state(null); // { ok, latency_ms, model, error }
  let embedTest = $state(null); // { ok, dim, latency_ms, model, error }
  let codeEmbedTest = $state(null); // { ok, dim, latency_ms, model, error }
  let testingLLM = $state(false);
  let testingEmbed = $state(false);
  let testingCodeEmbed = $state(false);

  // Langfuse (LLM observability). Saved on its own so changing an export
  // setting does not push the whole Docs & RAG form, and never reindexes.
  let langfuseKey = $state(""); // write-only; blank means "keep existing"
  let langfuseTest = $state(null); // { ok, latency_ms, model (project), error }
  let testingLangfuse = $state(false);
  let langfuseBusy = $state(false);
  let langfuseMsg = $state("");
  let langfuseErr = $state("");

  function logTestFailure(name, result) {
    if (result && !result.ok) {
      console.error(`[krabby] ${name} test failed`, result);
    }
  }

  function adoptServerCfg(cfg) {
    serverCfg = normalizeSettingsSnapshot(cfg);
    return serverCfg;
  }

  function initializeDrafts(cfg) {
    const snapshot = adoptServerCfg(cfg);
    if (!snapshot) return;
    runtimeDraft = createRuntimeDraft(snapshot);
    docsDraft = createDocsDraft(snapshot);
    langfuseDraft = createLangfuseDraft(snapshot);
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
      initializeDrafts(await api.docsConfig());
    } catch (e) {
      docsErr = e.message;
    }

    try {
      const ns = await api.namespaces();
      namespaceOptions = Array.isArray(ns) ? ns : [];
    } catch {
      namespaceOptions = [];
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

  function docsPayload() {
    return buildDocsPayload(docsDraft, { llm: llmKey, embed: embedKey, codeEmbed: codeEmbedKey });
  }

  async function saveDocs() {
    saving = true;
    docsErr = "";
    docsMsg = "";
    try {
      const snapshot = adoptServerCfg(await api.setDocsConfig(docsPayload()));
      docsDraft = createDocsDraft(snapshot);
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
    if (!runtimeDraft) return;
    runtimeBusy = true;
    runtimeMsg = "";
    runtimeErr = "";
    try {
      const patch = buildRuntimePayload(runtimeDraft, webhookSecret, clearWebhook);
      const snapshot = adoptServerCfg(await api.setDocsConfig(patch));
      runtimeDraft = createRuntimeDraft(snapshot);
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
      llmTest = await api.testLLM(docsPayload());
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
      embedTest = await api.testEmbedder(docsPayload());
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
      codeEmbedTest = await api.testCodeEmbedder(docsPayload());
      logTestFailure("code embedder", codeEmbedTest);
    } catch (e) {
      codeEmbedTest = { ok: false, error: e.message };
      console.error("[krabby] Code embedder test request failed", e);
    } finally {
      testingCodeEmbed = false;
    }
  }

  async function saveLangfuse() {
    langfuseBusy = true;
    langfuseErr = "";
    langfuseMsg = "";
    try {
      const snapshot = adoptServerCfg(await api.setDocsConfig(buildLangfusePayload(langfuseDraft, langfuseKey)));
      langfuseDraft = createLangfuseDraft(snapshot);
      langfuseKey = "";
      langfuseMsg = langfuseDraft.langfuse_enabled
        ? "Saved. Traces export from the next model call."
        : "Saved. Langfuse export is off.";
      successToast("Saved");
    } catch (e) {
      langfuseErr = e.message;
    } finally {
      langfuseBusy = false;
    }
  }

  async function testLangfuse() {
    testingLangfuse = true;
    langfuseTest = null;
    try {
      langfuseTest = await api.testLangfuse(buildLangfusePayload(langfuseDraft, langfuseKey));
      logTestFailure("Langfuse", langfuseTest);
    } catch (e) {
      langfuseTest = { ok: false, error: e.message };
      console.error("[krabby] Langfuse test request failed", e);
    } finally {
      testingLangfuse = false;
    }
  }

  function useDefaultPrompt() {
    docsDraft.docs_prompt = docsDraft.docs_default_prompt;
    promptView = "custom";
  }

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
      ["MCP core path", s.mcp.path],
      ["MCP API path", `${s.mcp.path}/api`],
      ["MCP admin path", `${s.mcp.path}/admin`],
      ["Graphify bin", s.graphify.bin],
      ["Graphify version", s.graphify.version || "unknown"],
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
        {#each rows(settings) as [label, value, isBool] (label)}
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
          {#each creds as c (c.pattern)}
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

{#if serverCfg && runtimeDraft}
  <RuntimeSettings
    bind:draft={runtimeDraft}
    bind:webhookSecret
    {namespaceOptions}
    busy={runtimeBusy}
    message={runtimeMsg}
    error={runtimeErr}
    onSave={saveRuntime}
  />
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

{#if serverCfg && docsDraft && langfuseDraft}
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
      <input type="checkbox" bind:checked={docsDraft.docs_enabled} />
      Generate markdown docs on refresh
    </label>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        LLM base URL
        <input class="input" bind:value={docsDraft.llm_base_url} placeholder="https://api.openai.com/v1" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        LLM model
        <input class="input" bind:value={docsDraft.llm_model} placeholder="gpt-4o-mini" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        LLM API key {docsDraft.llm_api_key_set ? "(set)" : "(not set)"}
        <input class="input" type="password" bind:value={llmKey} placeholder="leave blank to keep" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Doc concurrency
        <input class="input" type="number" bind:value={docsDraft.docs_concurrency} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Max summary groups
        <input class="input" type="number" bind:value={docsDraft.docs_max_groups} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Summary model (fast; blank = main model)
        <input class="input" bind:value={docsDraft.docs_summary_model} placeholder="e.g. google-ai/gemini-2.5-flash" />
      </label>
    </div>

    <!-- Vision analysis for images in web pages -->
    <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Web image analysis</div>
    <p class="mb-3 text-[12px] text-faint">
      Optionally describe useful images while importing web pages. When enabled, image bytes, including
      images from private pages if allowed below, may be sent to the configured vision provider. Keep this
      off unless that provider is approved to receive the source content.
    </p>
    <label class="mb-3 flex items-start gap-2 text-[13px]">
      <input class="mt-1" type="checkbox" bind:checked={docsDraft.web_image_analysis_enabled} />
      <span>
        Analyze images with a vision model
        <span class="block text-[12px] text-faint">Disabled by default. The configured LLM endpoint and credentials are used.</span>
      </span>
    </label>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Vision model
        <input class="input" bind:value={docsDraft.web_image_model} placeholder="blank = main LLM model" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Maximum images per page
        <input class="input" type="number" min="1" max="50" bind:value={docsDraft.web_image_max_per_page} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Maximum bytes per image
        <input class="input" type="number" min="1" max="33554432" bind:value={docsDraft.web_image_max_bytes} />
        <span class="text-[12px] text-faint">Default 4 MiB (4194304 bytes).</span>
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Maximum decoded pixels
        <input class="input" type="number" min="1" max="100000000" bind:value={docsDraft.web_image_max_pixels} />
        <span class="text-[12px] text-faint">Default 16 megapixels (16000000 pixels).</span>
      </label>
    </div>
    <label class="mt-3 flex items-start gap-2 text-[13px]">
      <input class="mt-1" type="checkbox" bind:checked={docsDraft.web_image_allow_authenticated} />
      <span>
        Allow authenticated and private-network images
        <span class="block text-[12px] text-faint">
          Off by default. Private-network access is limited to each explicitly configured source origin.
        </span>
      </span>
    </label>

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
          bind:value={docsDraft.docs_prompt}
          placeholder="Leave blank to use the built-in default prompt."
        ></textarea>
      {:else}
        <textarea
          class="input bg-surface-2 font-mono text-[12px]"
          rows="12"
          readonly
          value={docsDraft.docs_default_prompt}
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
        <input class="input" bind:value={docsDraft.embed_base_url} placeholder="http://localhost:11434/v1" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Embedder model
        <input class="input" bind:value={docsDraft.embed_model} placeholder="nomic-embed-text" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Embedder API key {docsDraft.embed_api_key_set ? "(set)" : "(not set)"}
        <input class="input" type="password" bind:value={embedKey} placeholder="leave blank to keep" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Embedding dim (0 = model default)
        <input class="input" type="number" bind:value={docsDraft.embed_dim} />
      </label>
    </div>
    <p class="mt-2 text-[12px] text-faint">
      The dim is requested from the provider. On a Matryoshka model (Gemini Embedding 2 accepts
      128–3072, text-embedding-3 likewise) a narrower vector keeps most of its accuracy and cuts
      vector memory proportionally. Providers that do not support it stay at their native width —
      test the embedder and check the reported dim. Changing the dim rebuilds the index.
    </p>

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
      <input type="checkbox" bind:checked={docsDraft.code_rag_enabled} />
      Enable semantic code search
    </label>
    <p class="mb-3 text-[12px] text-faint">
      Normal code search always uses the local bw full-text index. Enable this option for vector-based
      semantic search; leave the code embedder URL blank to reuse the docs embedder.
    </p>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code embedder base URL
        <input class="input" bind:value={docsDraft.code_embed_base_url} placeholder="https://api.mistral.ai/v1" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code embedder model
        <input class="input" bind:value={docsDraft.code_embed_model} placeholder="codestral-embed-2505" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code embedder API key {docsDraft.code_embed_api_key_set ? "(set)" : "(not set)"}
        <input class="input" type="password" bind:value={codeEmbedKey} placeholder="leave blank to keep" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code embedding dim (0 = model default)
        <input class="input" type="number" bind:value={docsDraft.code_embed_dim} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code chunk size (chars)
        <input class="input" type="number" bind:value={docsDraft.code_rag_chunk_size} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code chunk overlap (chars)
        <input class="input" type="number" bind:value={docsDraft.code_rag_chunk_overlap} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Code snippets returned (top_k)
        <input class="input" type="number" bind:value={docsDraft.code_rag_top_k} />
      </label>
    </div>

    <!-- Retrieval -->
    <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Retrieval</div>
    <label class="mb-3 flex items-center gap-2 text-[13px]">
      <input type="checkbox" bind:checked={docsDraft.rag_enabled} />
      Enable RAG indexing &amp; retrieval
    </label>
    <p class="mb-3 text-[12px] text-faint">Vectors are stored locally in embedded bw indexes.</p>
    <label class="mb-3 flex items-start gap-2 text-[13px]">
      <input class="mt-1" type="checkbox" bind:checked={docsDraft.rag_keep_markdown_targets} />
      <span>
        Keep link and image URLs in search indexes
        <span class="block text-[12px] text-faint">
          When disabled, link labels and image alt text remain searchable but their destination URLs are omitted.
        </span>
      </span>
    </label>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Docs returned (top_docs)
        <input class="input" type="number" min="1" max="20" bind:value={docsDraft.rag_top_docs} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Chunk size (chars)
        <input class="input" type="number" bind:value={docsDraft.rag_chunk_size} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Chunk overlap (chars)
        <input class="input" type="number" bind:value={docsDraft.rag_chunk_overlap} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Chunk matches (top_k)
        <input class="input" type="number" bind:value={docsDraft.rag_top_k} />
      </label>
    </div>

    <!-- Hybrid search -->
    <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Hybrid search</div>
    <p class="mb-3 text-[12px] text-faint">
      Hybrid mode fuses the BM25 and semantic rankings with weighted reciprocal rank fusion.
      Both rankers are always asked for the same candidate depth; weight a ranker here rather
      than by changing depth.
    </p>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Candidates per ranker
        <input class="input" type="number" bind:value={docsDraft.rag_hybrid_candidates} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        RRF k
        <input class="input" type="number" bind:value={docsDraft.rag_hybrid_rrf_k} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Lexical weight
        <input class="input" type="number" step="0.1" bind:value={docsDraft.rag_hybrid_weight_lexical} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Semantic weight
        <input class="input" type="number" step="0.1" bind:value={docsDraft.rag_hybrid_weight_semantic} />
      </label>
    </div>
    <label class="mt-3 flex flex-col gap-1 text-[13px] text-dim">
      Lexical stop words (comma separated)
      <input class="input" type="text" bind:value={docsDraft.rag_lexical_stop_words_text} />
    </label>
    <p class="mt-1 text-[12px] text-faint">
      Empty by default, and on purpose: BM25 already scores a word that appears in most
      documents near zero, in any language, so this is a query-latency knob and not a
      relevance one. Set it to your own corpus language's function words only if lexical
      search is slow on a large corpus.
    </p>

    <div class="mt-6">
      <button class="btn btn-primary" onclick={saveDocs} disabled={saving}>
        {saving ? "Saving…" : "Save & rebuild"}
      </button>
    </div>
  </div>

  <LangfuseSettings
    bind:draft={langfuseDraft}
    bind:secretKey={langfuseKey}
    testResult={langfuseTest}
    testing={testingLangfuse}
    busy={langfuseBusy}
    message={langfuseMsg}
    error={langfuseErr}
    onTest={testLangfuse}
    onSave={saveLangfuse}
  />
{:else if !docsErr}
  <div class="mt-4 text-dim">Loading…</div>
{/if}
