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
  let stopWordsText = $state(""); // rag_lexical_stop_words as a comma-separated field
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

  // adoptDocsCfg is the single place docsCfg is replaced by a server response.
  //
  // A Go nil slice marshals to null, not [], so any list the user has never
  // touched arrives as null — and the template indexes repo_schedules
  // directly. Every assignment therefore has to normalize, which is exactly
  // what a partial save (the observability form, which sends no list fields at
  // all) used to skip.
  function adoptDocsCfg(cfg) {
    if (!cfg) return;
    if (!Array.isArray(cfg.repo_schedules)) cfg.repo_schedules = [];
    if (typeof cfg.rag_keep_markdown_targets !== "boolean") cfg.rag_keep_markdown_targets = false;
    if (typeof cfg.web_image_analysis_enabled !== "boolean") cfg.web_image_analysis_enabled = false;
    if (typeof cfg.web_image_allow_authenticated !== "boolean") cfg.web_image_allow_authenticated = false;
    if (!cfg.web_image_max_per_page) cfg.web_image_max_per_page = 3;
    if (!cfg.web_image_max_bytes) cfg.web_image_max_bytes = 4 * 1024 * 1024;
    if (!cfg.web_image_max_pixels) cfg.web_image_max_pixels = 16000000;
    docsCfg = cfg;
    stopWordsText = (cfg.rag_lexical_stop_words ?? []).join(", ");
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
      adoptDocsCfg(await api.docsConfig());
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

  // Lexical stop words are edited as one comma-separated field but stored as a
  // list. Empty entries are dropped so a trailing comma is harmless.
  function parseStopWords(text) {
    return (text ?? "")
      .split(",")
      .map((w) => w.trim())
      .filter(Boolean);
  }

  // Repository poll schedule editor. Each schedule targets a namespace ("*" =
  // all, "" / "default" = untagged repos) and lists one or more cron specs;
  // every spec triggers a poll of that namespace. Deep $state reactivity makes
  // in-place push/splice update the UI.
  function addSchedule() {
    docsCfg.repo_schedules.push({ namespace: "*", specs: ["0 * * * *"], disabled: false });
  }
  function removeSchedule(i) {
    docsCfg.repo_schedules.splice(i, 1);
  }
  function addSpec(i) {
    docsCfg.repo_schedules[i].specs.push("");
  }
  function removeSpec(i, j) {
    docsCfg.repo_schedules[i].specs.splice(j, 1);
    if (docsCfg.repo_schedules[i].specs.length === 0) docsCfg.repo_schedules[i].specs.push("");
  }

  // cleanSchedules normalizes the editor state for the API: trims specs, drops
  // empty ones, and removes schedules left without any spec.
  function cleanSchedules(list) {
    return (list || [])
      .map((s) => ({
        namespace: (s.namespace || "").trim(),
        specs: (s.specs || []).map((x) => (x || "").trim()).filter(Boolean),
        disabled: !!s.disabled,
      }))
      .filter((s) => s.specs.length > 0);
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
    delete patch.langfuse_secret_key_set;
    delete patch.docs_default_prompt;
    delete patch.updated_at;
    // Never submit half-edited (empty-spec) schedules the backend would reject.
    patch.repo_schedules = cleanSchedules(docsCfg.repo_schedules);
    patch.rag_lexical_stop_words = parseStopWords(stopWordsText);
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
      adoptDocsCfg(await api.setDocsConfig(buildPatch()));
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
      const schedules = cleanSchedules(docsCfg.repo_schedules);
      const patch = {
        task_concurrency: Number(docsCfg.task_concurrency),
        repo_schedules: schedules,
      };
      docsCfg.repo_schedules = schedules;
      if (clearWebhook) patch.webhook_secret = "";
      else if (webhookSecret) patch.webhook_secret = webhookSecret;
      adoptDocsCfg(await api.setDocsConfig(patch));
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

  // buildLangfusePatch sends only the observability fields. Keeping it narrow
  // is what lets the server recognise the change as observability-only and skip
  // the reindex an ordinary settings save triggers.
  function buildLangfusePatch() {
    return {
      langfuse_enabled: !!docsCfg.langfuse_enabled,
      langfuse_host: docsCfg.langfuse_host || "",
      langfuse_public_key: docsCfg.langfuse_public_key || "",
      langfuse_secret_key: langfuseKey,
      langfuse_environment: docsCfg.langfuse_environment || "",
      langfuse_capture: docsCfg.langfuse_capture || "full",
      langfuse_max_content_bytes: Number(docsCfg.langfuse_max_content_bytes) || 0,
      langfuse_trace_docs: !!docsCfg.langfuse_trace_docs,
      langfuse_trace_embed: !!docsCfg.langfuse_trace_embed,
      langfuse_trace_mcp: !!docsCfg.langfuse_trace_mcp,
      langfuse_trace_http: !!docsCfg.langfuse_trace_http,
    };
  }

  async function saveLangfuse() {
    langfuseBusy = true;
    langfuseErr = "";
    langfuseMsg = "";
    try {
      adoptDocsCfg(await api.setDocsConfig(buildLangfusePatch()));
      langfuseKey = "";
      langfuseMsg = docsCfg.langfuse_enabled
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
      langfuseTest = await api.testLangfuse(buildLangfusePatch());
      logTestFailure("Langfuse", langfuseTest);
    } catch (e) {
      langfuseTest = { ok: false, error: e.message };
      console.error("[krabby] Langfuse test request failed", e);
    } finally {
      testingLangfuse = false;
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
    to expose credential and docs/RAG administration tools.
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
    <!-- Repository poll schedules (cron, per namespace) -->
    <div class="mb-4">
      <div class="mb-1 flex items-center justify-between">
        <span class="text-[13px] font-semibold text-dim">Repository poll schedules</span>
        <button class="btn btn-sm" onclick={addSchedule}>+ Add schedule</button>
      </div>
      <p class="mb-2 text-[12px] text-faint">
        Poll repositories on cron schedules. Target a namespace (<code class="font-mono">*</code> = all,
        <code class="font-mono">default</code> = untagged) and add one or more cron specs; each spec
        triggers a poll. Multiple schedules and specs are supported. With no schedules configured, polling
        falls back to the legacy fixed interval.
      </p>

      <datalist id="ns-options">
        <option value="*"></option>
        <option value="default"></option>
        {#each namespaceOptions as ns}
          <option value={ns.namespace}></option>
        {/each}
      </datalist>

      {#if docsCfg.repo_schedules.length === 0}
        <p class="rounded-md border border-dashed border-faint px-3 py-2 text-[12px] text-faint">
          No schedules configured — repositories poll on the legacy fixed interval.
        </p>
      {/if}

      {#each docsCfg.repo_schedules as sched, i}
        <div class="mb-2 rounded-md border border-faint p-3">
          <div class="flex flex-wrap items-end gap-3">
            <label class="flex flex-col gap-1 text-[12px] text-dim">
              Namespace
              <input
                class="input w-48"
                list="ns-options"
                bind:value={sched.namespace}
                placeholder="* (all namespaces)"
              />
            </label>
            <label class="flex items-center gap-2 text-[12px] text-dim">
              <input type="checkbox" bind:checked={sched.disabled} />
              Disabled
            </label>
            <button class="btn btn-sm btn-danger ml-auto" onclick={() => removeSchedule(i)}>
              Remove schedule
            </button>
          </div>
          <div class="mt-2 flex flex-col gap-1.5">
            {#each sched.specs as _spec, j}
              <div class="flex items-center gap-2">
                <input
                  class="input font-mono text-[12px]"
                  bind:value={sched.specs[j]}
                  placeholder="0 * * * *  (or @every 15m)"
                />
                <button class="btn btn-sm" onclick={() => removeSpec(i, j)} title="Remove cron spec">
                  −
                </button>
              </div>
            {/each}
            <button class="btn btn-sm self-start" onclick={() => addSpec(i)}>+ Add cron spec</button>
          </div>
        </div>
      {/each}
    </div>

    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
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

    <!-- Vision analysis for images in web pages -->
    <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Web image analysis</div>
    <p class="mb-3 text-[12px] text-faint">
      Optionally describe useful images while importing web pages. When enabled, image bytes, including
      images from private pages if allowed below, may be sent to the configured vision provider. Keep this
      off unless that provider is approved to receive the source content.
    </p>
    <label class="mb-3 flex items-start gap-2 text-[13px]">
      <input class="mt-1" type="checkbox" bind:checked={docsCfg.web_image_analysis_enabled} />
      <span>
        Analyze images with a vision model
        <span class="block text-[12px] text-faint">Disabled by default. The configured LLM endpoint and credentials are used.</span>
      </span>
    </label>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Vision model
        <input class="input" bind:value={docsCfg.web_image_model} placeholder="blank = main LLM model" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Maximum images per page
        <input class="input" type="number" min="1" max="50" bind:value={docsCfg.web_image_max_per_page} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Maximum bytes per image
        <input class="input" type="number" min="1" max="33554432" bind:value={docsCfg.web_image_max_bytes} />
        <span class="text-[12px] text-faint">Default 4 MiB (4194304 bytes).</span>
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Maximum decoded pixels
        <input class="input" type="number" min="1" max="100000000" bind:value={docsCfg.web_image_max_pixels} />
        <span class="text-[12px] text-faint">Default 16 megapixels (16000000 pixels).</span>
      </label>
    </div>
    <label class="mt-3 flex items-start gap-2 text-[13px]">
      <input class="mt-1" type="checkbox" bind:checked={docsCfg.web_image_allow_authenticated} />
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
        Embedding dim (0 = model default)
        <input class="input" type="number" bind:value={docsCfg.embed_dim} />
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
        Code embedding dim (0 = model default)
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
    <label class="mb-3 flex items-start gap-2 text-[13px]">
      <input class="mt-1" type="checkbox" bind:checked={docsCfg.rag_keep_markdown_targets} />
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
        <input class="input" type="number" min="1" max="20" bind:value={docsCfg.rag_top_docs} />
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
        <input class="input" type="number" bind:value={docsCfg.rag_hybrid_candidates} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        RRF k
        <input class="input" type="number" bind:value={docsCfg.rag_hybrid_rrf_k} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Lexical weight
        <input class="input" type="number" step="0.1" bind:value={docsCfg.rag_hybrid_weight_lexical} />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Semantic weight
        <input class="input" type="number" step="0.1" bind:value={docsCfg.rag_hybrid_weight_semantic} />
      </label>
    </div>
    <label class="mt-3 flex flex-col gap-1 text-[13px] text-dim">
      Lexical stop words (comma separated)
      <input class="input" type="text" bind:value={stopWordsText} />
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

  <!-- LLM observability (Langfuse) -->
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
        <input type="checkbox" bind:checked={docsCfg.langfuse_enabled} />
        Enable Langfuse export
      </label>
      <span class="flex items-center gap-2">
        {#if langfuseTest}
          {#if langfuseTest.ok}
            <span class="text-[12px] text-ok">
              ✓ ok{langfuseTest.model ? ` · project ${langfuseTest.model}` : ""} · {langfuseTest.latency_ms}ms
            </span>
          {:else}
            <span class="max-w-[24rem] truncate text-[12px] text-err" title={langfuseTest.error}>✗ {langfuseTest.error}</span>
          {/if}
        {/if}
        <button class="btn btn-sm" onclick={testLangfuse} disabled={testingLangfuse}>
          {testingLangfuse ? "Testing…" : "Test connection"}
        </button>
      </span>
    </div>

    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Host
        <input class="input" bind:value={docsCfg.langfuse_host} placeholder="https://cloud.langfuse.com" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Environment
        <input class="input" bind:value={docsCfg.langfuse_environment} placeholder="production" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Public key
        <input class="input" bind:value={docsCfg.langfuse_public_key} placeholder="pk-lf-…" />
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Secret key {docsCfg.langfuse_secret_key_set ? "(set)" : "(not set)"}
        <input class="input" type="password" bind:value={langfuseKey} placeholder="leave blank to keep" />
      </label>
    </div>
    <p class="mt-2 text-[12px] text-faint">
      For the EU region use <code>https://cloud.langfuse.com</code>; US, Japan and HIPAA have their
      own hosts. A self-hosted instance needs v3.22.0 or newer for the OTLP endpoint.
    </p>

    <!-- What gets traced -->
    <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">What gets traced</div>
    <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
      <label class="flex items-start gap-2 text-[13px]">
        <input type="checkbox" class="mt-1" bind:checked={docsCfg.langfuse_trace_docs} />
        <span>
          Documentation LLM calls
          <span class="block text-[12px] text-faint">
            One trace per docs build, one generation per summary group plus the synthesis.
          </span>
        </span>
      </label>
      <label class="flex items-start gap-2 text-[13px]">
        <input type="checkbox" class="mt-1" bind:checked={docsCfg.langfuse_trace_embed} />
        <span>
          Embedding calls
          <span class="block text-[12px] text-faint">
            One observation per Embed call, not per batch — a large index would otherwise emit
            thousands.
          </span>
        </span>
      </label>
      <label class="flex items-start gap-2 text-[13px]">
        <input type="checkbox" class="mt-1" bind:checked={docsCfg.langfuse_trace_mcp} />
        <span>
          MCP tool calls
          <span class="block text-[12px] text-faint">
            Shows what a connected agent actually asked for. No model or token data.
          </span>
        </span>
      </label>
      <label class="flex items-start gap-2 text-[13px]">
        <input type="checkbox" class="mt-1" bind:checked={docsCfg.langfuse_trace_http} />
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

    <!-- Content capture -->
    <div class="mb-2 mt-6 text-[13px] font-semibold text-dim">Prompt &amp; completion capture</div>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Capture mode
        <select class="input" bind:value={docsCfg.langfuse_capture}>
          <option value="full">full — send prompts and replies whole</option>
          <option value="truncated">truncated — clip to 8 KiB</option>
          <option value="off">off — metadata only</option>
        </select>
      </label>
      <label class="flex flex-col gap-1 text-[13px] text-dim">
        Max bytes per value (0 = no limit)
        <input class="input" type="number" min="0" bind:value={docsCfg.langfuse_max_content_bytes} />
      </label>
    </div>

    {#if docsCfg.langfuse_enabled && docsCfg.langfuse_capture === "full"}
      <div class="mt-3 rounded border border-warn/40 bg-warn/10 p-3 text-[12px]">
        <div class="mb-1 font-semibold text-warn">What "full" capture means</div>
        <ul class="list-disc space-y-1 pl-4 text-dim">
          <li>
            <b>Your source code leaves this process.</b> Summary prompts embed the files being
            documented, so private repository contents are sent to
            {docsCfg.langfuse_host || "the configured Langfuse instance"} verbatim. On Langfuse Cloud
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

    {#if langfuseErr}<div class="mt-3 text-[13px] text-err">{langfuseErr}</div>{/if}
    {#if langfuseMsg}<div class="mt-3 text-[13px] text-ok">{langfuseMsg}</div>{/if}

    <div class="mt-6">
      <button class="btn btn-primary" onclick={saveLangfuse} disabled={langfuseBusy}>
        {langfuseBusy ? "Saving…" : "Save observability settings"}
      </button>
    </div>
  </div>
{:else if !docsErr}
  <div class="mt-4 text-dim">Loading…</div>
{/if}
