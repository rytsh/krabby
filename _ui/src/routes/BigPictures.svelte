<script>
  import { onMount, onDestroy } from "svelte";
  import { push } from "svelte-spa-router";
  import { link } from "../lib/router.js";
  import { api } from "../lib/api.js";
  import { createLatestRequest } from "../lib/async.js";
  import { pictureURL } from "../lib/big-picture.js";
  import BigPictureForm from "../components/BigPictureForm.svelte";

  let result = $state({ items: [], total: 0, page: 1, per_page: 20 });
  let namespace = $state("*");
  let query = $state("");
  let loading = $state(true);
  let error = $state("");
  let creating = $state(false);
  const requests = createLatestRequest();

  async function load(page = 1) {
    const request = requests.nextAbortable();
    loading = true;
    error = "";
    try {
      const data = await api.bigPictures({ namespace, q: query, page }, request.signal);
      if (request.isLatest()) result = data;
    } catch (e) { if (request.isLatest() && e.name !== "AbortError") error = e.message; }
    finally { if (request.isLatest()) loading = false; }
  }
  onMount(() => { void load(); });
  onDestroy(() => requests.invalidate());
</script>

<div class="mb-4 flex items-start justify-between gap-3"><div><h2 class="mb-1 text-[15px] font-semibold">Architecture workspaces</h2><p class="text-dim">Big Pictures connect multiple repositories and external sources into versioned, folder-like document trees.</p></div><button class="btn btn-primary shrink-0" onclick={() => { creating = true; }}>New Big Picture</button></div>
{#if creating}<div class="mb-4"><BigPictureForm onSaved={(picture) => push(pictureURL(picture.name))} onCancel={() => { creating = false; }} /></div>{/if}
<form class="mb-4 flex flex-wrap gap-2" onsubmit={(event) => { event.preventDefault(); load(); }}><input class="input w-40" aria-label="Namespace filter" bind:value={namespace} placeholder="* = all namespaces" /><input class="input min-w-0 flex-1" aria-label="Search Big Pictures" bind:value={query} placeholder="Search names, titles and descriptions…" /><button class="btn" type="submit">Search</button></form>
{#if error}<p class="mb-3 text-danger" role="alert">{error}</p>{/if}
{#if loading}<p class="text-dim">Loading…</p>
{:else}
  <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
    {#each result.items as picture (picture.name)}
      <a class="card block p-4 transition-colors hover:bg-surface-2" href={pictureURL(picture.name)} use:link>
        <span class="text-[11px] text-faint">{picture.namespace}</span><h3 class="mt-1 text-[15px] font-semibold">{picture.title}</h3><p class="mt-1 font-mono text-[12px] text-dim">{picture.name}</p><p class="mt-2 text-[13px] text-dim">{picture.description}</p><p class="mt-3 text-[12px] text-faint">{picture.source_count} sources · {picture.document_count} documents · {picture.current_revision ? (picture.stale ? "Config changed since publication" : "Published") : "Not published yet"}</p>
      </a>
    {:else}<p class="col-span-full p-6 text-center text-dim">No Big Pictures in this scope.</p>{/each}
  </div>
  <div class="mt-4 flex items-center justify-between text-[12px] text-dim"><span>{result.total} workspaces · page {result.page}</span><div class="flex gap-2"><button class="btn btn-sm" disabled={result.page <= 1} onclick={() => load(result.page - 1)}>Previous</button><button class="btn btn-sm" disabled={result.page * result.per_page >= result.total} onclick={() => load(result.page + 1)}>Next</button></div></div>
{/if}
