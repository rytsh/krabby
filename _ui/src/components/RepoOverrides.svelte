<script>
  import {
    createRepoOverridesDraft,
    hasRepoOverrides,
    repoSettingsRows,
    skippableRepoStages,
  } from "../lib/repo-overrides.js";

  let {
    repo,
    settings,
    form = $bindable(),
    open = $bindable(false),
    saving = false,
    onSave,
  } = $props();

  function toggleEditor() {
    if (open) {
      open = false;
      return;
    }
    form = createRepoOverridesDraft(repo);
    open = true;
  }

  function toggleSkip(key) {
    const selected = form.skip_stages.includes(key)
      ? form.skip_stages.filter((stage) => stage !== key)
      : [...form.skip_stages, key];
    form = { ...form, skip_stages: selected };
  }
</script>

<div class="card shrink-0 flex flex-col gap-2.5 p-4 text-[13px]">
  <div class="flex items-center justify-between gap-2">
    <span class="text-dim">Build settings</span>
    <button class="btn btn-sm" onclick={toggleEditor}>
      {open ? "Close" : "Edit"}
    </button>
  </div>

  {#if !open}
    {#if settings}
      {#each repoSettingsRows(settings) as row (row.label)}
        <div class="flex items-start justify-between gap-2">
          <span class="text-dim">{row.label}</span>
          <span class="text-right">
            <span class="break-all font-mono text-faint">{row.value}</span>
            {#if row.overridden}
              <span class="ml-1 text-[11px] text-acc">repo</span>
            {/if}
          </span>
        </div>
      {/each}
      <p class="m-0 text-[12px] text-faint">
        {hasRepoOverrides(repo)
          ? "Values marked “repo” override the global settings."
          : "All values come from the global settings."}
      </p>
    {:else}
      <p class="m-0 text-[12px] text-faint">Loading…</p>
    {/if}
  {:else}
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Also index these (added to the defaults)
      <input class="input" placeholder="**/*.yaml, **/*.yml" bind:value={form.include_extra} />
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Index only these (replaces the defaults)
      <input class="input" placeholder="empty = built-in allowlist" bind:value={form.include} />
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Skip these
      <input class="input" placeholder="**/generated/**" bind:value={form.exclude} />
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Keep out of the knowledge graph
      <input class="input" placeholder="proto/, **/*.gen.go" bind:value={form.graph_exclude} />
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Extra documentation instructions
      <textarea
        class="input min-h-[80px]"
        placeholder="Environments are separate compose files; render a markdown table of service, image and version per environment."
        bind:value={form.docs_prompt_extra}
      ></textarea>
    </label>
    <div class="flex flex-col gap-1 text-[13px] text-dim">
      Documentation input budget, bytes (empty = global default)
      <div class="flex flex-wrap gap-2">
        <input
          class="input flex-1"
          placeholder="per file, default 49152"
          bind:value={form.docs_max_source_bytes}
        />
        <input
          class="input flex-1"
          placeholder="per summary call, default 98304"
          bind:value={form.docs_max_group_bytes}
        />
        <input
          class="input flex-1"
          placeholder="final synthesis, default 262144"
          bind:value={form.docs_max_synthesis_bytes}
        />
      </div>
      <span class="text-[12px] text-faint">
        Nothing past the per-file budget is ever sent to the model, so a repo whose substance
        sits in a few very large files is documented from a truncated prefix until this is
        raised. Raise the per-call budget with it, or that one binds first.
      </span>
    </div>
    <div class="flex flex-col gap-1 text-[13px] text-dim">
      Stages this repository does not run
      <div class="flex flex-wrap gap-3">
        {#each skippableRepoStages as stage (stage.key)}
          <label class="flex items-center gap-1 text-[13px]">
            <input
              type="checkbox"
              checked={form.skip_stages.includes(stage.key)}
              onchange={() => toggleSkip(stage.key)}
            />
            {stage.label}
          </label>
        {/each}
      </div>
      <span class="text-[12px] text-faint">
        Dependents still run without a skipped graph, just without symbol anchoring. Asking for
        a skipped stage from the Generate buttons is rejected rather than silently doing nothing.
      </span>
    </div>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Replace the documentation prompt
      <textarea
        class="input min-h-[60px]"
        placeholder="empty = keep the default prompt"
        bind:value={form.docs_prompt}
      ></textarea>
    </label>
    <p class="m-0 text-[12px] text-faint">
      Prefer the two “extra” fields: they add to the defaults. The replacing fields drop the
      built-in allowlist and the default prompt’s formatting rules for this repo. Saving re-indexes
      and re-documents this repository; changing the graph ignores rebuilds its graph too.
    </p>
    <div class="flex gap-2">
      <button class="btn btn-primary" onclick={onSave} disabled={saving}>
        {saving ? "Saving…" : "Save settings"}
      </button>
      <button class="btn" onclick={() => (open = false)}>Cancel</button>
    </div>
  {/if}
</div>
