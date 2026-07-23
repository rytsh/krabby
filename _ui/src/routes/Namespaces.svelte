<script>
  // Namespaces group tracked repositories. Each repo belongs to exactly one
  // namespace (untagged repos fall under "default"). A namespace can carry a
  // description that is surfaced to LLMs via the list_namespaces MCP tool so
  // they can pick the right search scope. This page manages those descriptions;
  // the per-repo tag is set on the repository itself (Repositories page / API).
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { successToast } from "../lib/toast.js";
  import Icon from "../lib/Icon.svelte";

  let namespaces = $state([]);
  let loaded = $state(false);
  let error = $state("");

  // Create/edit form. editing holds the original name while editing.
  let form = $state({ name: "", description: "" });
  let editing = $state("");
  let busy = $state(false);

  const DEFAULT = "default";

  async function load() {
    try {
      namespaces = (await api.namespaces()) || [];
      error = "";
    } catch (e) {
      error = e.message;
    } finally {
      loaded = true;
    }
  }

  function resetForm() {
    form = { name: "", description: "" };
    editing = "";
  }

  function startEdit(ns) {
    editing = ns.namespace;
    form = { name: ns.namespace, description: ns.description || "" };
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  async function save() {
    const name = form.name.trim();
    if (!name) return;
    busy = true;
    try {
      await api.upsertNamespace(name, form.description.trim());
      successToast(editing ? `Updated ${name}` : `Created ${name}`);
      resetForm();
      await load();
    } catch {
      // errorToast already fired in the api wrapper
    } finally {
      busy = false;
    }
  }

  async function remove(ns) {
    if (!confirm(`Delete the description for "${ns.namespace}"? Repositories keep their tag.`)) return;
    try {
      await api.deleteNamespace(ns.namespace);
      successToast(`Deleted description for ${ns.namespace}`);
      await load();
    } catch {
      // handled by api wrapper
    }
  }

  // The reserved "*" wildcard is not a real namespace and cannot be created.
  let nameInvalid = $derived(form.name.trim() === "*");

  onMount(load);
</script>

<h2 class="mb-1 text-[15px] font-semibold">Namespaces</h2>
<p class="mb-4 max-w-2xl text-dim">
  Namespaces group repositories for scoped search. A repository belongs to exactly one namespace;
  untagged repositories fall under <code class="font-mono">default</code>. MCP queries with no
  namespace search only <code class="font-mono">default</code>; pass
  <code class="font-mono">namespace: "*"</code> to search across all. Add a description so assistants
  know what each namespace holds and can pick the right scope.
</p>

<div class="card mb-4 grid grid-cols-1 gap-2 p-3 sm:grid-cols-[240px_1fr_auto]">
  <input
    class="input"
    placeholder="namespace name (e.g. payments)"
    bind:value={form.name}
    disabled={busy || !!editing}
  />
  <input
    class="input"
    placeholder="description — what this namespace holds"
    bind:value={form.description}
    disabled={busy}
    onkeydown={(e) => e.key === "Enter" && !nameInvalid && save()}
  />
  <div class="flex gap-2">
    <button class="btn btn-primary" onclick={save} disabled={busy || !form.name.trim() || nameInvalid}>
      {editing ? "Update" : "Create"}
    </button>
    {#if editing}
      <button class="btn" onclick={resetForm} disabled={busy}>Cancel</button>
    {/if}
  </div>
</div>
{#if nameInvalid}
  <p class="mb-3 text-[13px] text-err">"*" is reserved and cannot be used as a namespace name.</p>
{/if}

{#if error}
  <div class="card p-6 text-center text-err">{error}</div>
{:else if !loaded}
  <div class="mt-4 text-dim">Loading…</div>
{:else}
  <div class="card overflow-hidden">
    {#if namespaces.length === 0}
      <div class="p-6 text-center text-dim">No namespaces yet.</div>
    {:else}
      <table class="w-full border-collapse">
        <thead>
          <tr class="text-[13px] text-dim">
            <th class="border-b border-line px-4 py-2 text-left font-medium">Namespace</th>
            <th class="border-b border-line px-4 py-2 text-left font-medium">Repos</th>
            <th class="border-b border-line px-4 py-2 text-left font-medium">Description</th>
            <th class="border-b border-line px-4 py-2"></th>
          </tr>
        </thead>
        <tbody>
          {#each namespaces as ns}
            <tr class="hover:bg-surface-2">
              <td class="border-b border-line px-4 py-2.5">
                <span class="inline-flex items-center gap-1.5 font-mono text-[13px]">
                  <Icon name="tag" size={14} />{ns.namespace}
                </span>
              </td>
              <td class="border-b border-line px-4 py-2.5 text-[13px] text-faint">{ns.count}</td>
              <td class="border-b border-line px-4 py-2.5 text-[13px]">
                {ns.description || "—"}
              </td>
              <td class="border-b border-line px-4 py-2.5 text-right">
                <button class="btn btn-sm" onclick={() => startEdit(ns)}>Edit</button>
                {#if ns.namespace !== DEFAULT}
                  <button class="btn btn-sm btn-danger" onclick={() => remove(ns)}>Delete</button>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
{/if}
