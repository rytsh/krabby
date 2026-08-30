<script>
  import { SvelteMap } from "svelte/reactivity";

  let {
    draft = $bindable(),
    webhookSecret = $bindable(),
    namespaceOptions = [],
    busy = false,
    message = "",
    error = "",
    onSave,
  } = $props();

  const specKeysBySchedule = new SvelteMap();
  let nextSpecKey = 0;

  function keysFor(schedule) {
    if (!specKeysBySchedule.has(schedule)) {
      specKeysBySchedule.set(schedule, schedule.specs.map(() => ++nextSpecKey));
    }
    return specKeysBySchedule.get(schedule);
  }

  function specKey(schedule, index) {
    return keysFor(schedule)[index];
  }

  function addSchedule() {
    draft.repo_schedules.push({ namespace: "*", specs: ["0 * * * *"], disabled: false });
  }

  function removeSchedule(index) {
    draft.repo_schedules.splice(index, 1);
  }

  function addSpec(index) {
    const schedule = draft.repo_schedules[index];
    const keys = keysFor(schedule);
    schedule.specs.push("");
    keys.push(++nextSpecKey);
  }

  function removeSpec(scheduleIndex, specIndex) {
    const schedule = draft.repo_schedules[scheduleIndex];
    const keys = keysFor(schedule);
    schedule.specs.splice(specIndex, 1);
    keys.splice(specIndex, 1);
    if (schedule.specs.length === 0) {
      schedule.specs.push("");
      keys.push(++nextSpecKey);
    }
  }
</script>

<div class="card mt-3 p-4">
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
      {#each namespaceOptions as namespace (namespace.namespace)}
        <option value={namespace.namespace}></option>
      {/each}
    </datalist>

    {#if draft.repo_schedules.length === 0}
      <p class="rounded-md border border-dashed border-faint px-3 py-2 text-[12px] text-faint">
        No schedules configured — repositories poll on the legacy fixed interval.
      </p>
    {/if}

    {#each draft.repo_schedules as schedule, scheduleIndex (schedule)}
      <div class="mb-2 rounded-md border border-faint p-3">
        <div class="flex flex-wrap items-end gap-3">
          <label class="flex flex-col gap-1 text-[12px] text-dim">
            Namespace
            <input
              class="input w-48"
              list="ns-options"
              bind:value={schedule.namespace}
              placeholder="* (all namespaces)"
            />
          </label>
          <label class="flex items-center gap-2 text-[12px] text-dim">
            <input type="checkbox" bind:checked={schedule.disabled} />
            Disabled
          </label>
          <button class="btn btn-sm btn-danger ml-auto" onclick={() => removeSchedule(scheduleIndex)}>
            Remove schedule
          </button>
        </div>
        <div class="mt-2 flex flex-col gap-1.5">
          {#each schedule.specs as _spec, specIndex (specKey(schedule, specIndex))}
            <div class="flex items-center gap-2">
              <input
                class="input font-mono text-[12px]"
                bind:value={schedule.specs[specIndex]}
                placeholder="0 * * * *  (or @every 15m)"
              />
              <button
                class="btn btn-sm"
                onclick={() => removeSpec(scheduleIndex, specIndex)}
                title="Remove cron spec"
              >
                −
              </button>
            </div>
          {/each}
          <button class="btn btn-sm self-start" onclick={() => addSpec(scheduleIndex)}>+ Add cron spec</button>
        </div>
      </div>
    {/each}
  </div>

  <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Concurrent tasks
      <input class="input" type="number" min="1" max="64" bind:value={draft.task_concurrency} />
      <span class="text-[12px] text-faint">
        How many background tasks (refresh, generate, web sync, reindex) run at once. Lower to protect
        git/graphify/LLM/embedder backends; raise to process more repositories in parallel.
      </span>
    </label>
    <label class="flex flex-col gap-1 text-[13px] text-dim">
      Git webhook secret {draft.webhook_secret_set ? "(set)" : "(not set)"}
      <input
        class="input"
        type="password"
        bind:value={webhookSecret}
        placeholder="leave blank to keep existing"
      />
    </label>
  </div>
  <div class="mt-3 flex items-center gap-2">
    <button class="btn btn-primary" onclick={() => onSave(false)} disabled={busy}>Save runtime settings</button>
    <button
      class="btn btn-danger"
      onclick={() => onSave(true)}
      disabled={busy || !draft.webhook_secret_set}
    >Disable webhook verification</button>
    {#if message}<span class="text-[12px] text-ok">{message}</span>{/if}
    {#if error}<span class="text-[12px] text-err">{error}</span>{/if}
  </div>
</div>
