<!--
  RepoTree renders one level of the sidebar repository tree and recurses into
  itself for nested groups. A node is a folder (chevron + folder icon); when it
  is also a real owner group (node.owner != null) its repos are loaded lazily
  and listed once the folder is expanded, alongside any child folders. This is
  what makes ".../parser/poc" nest inside ".../parser" instead of sitting next
  to it.
-->
<script>
  import { link } from "./router.js";
  import { ownerRepos, ownerLoading } from "./repos.js";
  import { nameOf } from "./paths.js";
  import Icon from "./Icon.svelte";
  import Status from "./Status.svelte";
  import Self from "./RepoTree.svelte";

  let {
    nodes = [],
    depth = 0,
    expanded = {},
    onToggle,
    view,
    repoId,
  } = $props();

  // Indentation grows with depth; folder rows and repo rows share the same
  // per-level step so children line up under their parent's label.
  const step = 14;
  let pad = $derived(10 + depth * step);
</script>

{#each nodes as node (node.key)}
  <button
    class="flex w-full min-w-0 cursor-pointer items-center gap-1.5 rounded-md py-1.5 pr-2.5 text-left text-[13px] text-dim transition-colors hover:bg-surface-2 hover:text-fg"
    style={`padding-left:${pad}px`}
    onclick={() => onToggle(node)}
    aria-expanded={!!expanded[node.key]}
    title={node.owner || node.key}
  >
    <span class="inline-flex shrink-0"><Icon name={expanded[node.key] ? "chevron-down" : "chevron-right"} size={13} /></span>
    <span class="inline-flex shrink-0"><Icon name="folder" size={13} /></span>
    <span class="min-w-0 flex-1 truncate font-mono">{node.label}</span>
    {#if node.count > 0}
      <span class="ml-auto shrink-0 text-[11px] text-faint">{node.count}</span>
    {/if}
  </button>

  {#if expanded[node.key]}
    <!-- Child folders first, then this node's own repos (if it is an owner). -->
    {#if node.children.length > 0}
      <Self
        nodes={node.children}
        depth={depth + 1}
        {expanded}
        {onToggle}
        {view}
        {repoId}
      />
    {/if}

    {#if node.owner !== null}
      {#if $ownerLoading.has(node.owner) && !($ownerRepos[node.owner]?.length)}
        <div class="py-1.5 pr-2.5 text-[12px] text-faint" style={`padding-left:${pad + step + 20}px`}>
          Loading…
        </div>
      {:else}
        {#each $ownerRepos[node.owner] || [] as r (r.id)}
          <a
            href={`/repos/${r.id}`}
            use:link
            title={r.id}
            class="flex min-w-0 items-center gap-2 rounded-md py-1.5 pr-2.5 text-[13px] text-dim transition-colors hover:bg-surface-2 hover:text-fg"
            style={`padding-left:${pad + step + 20}px`}
            class:!bg-surface-2={view === "repo" && repoId === r.id}
            class:!text-fg={view === "repo" && repoId === r.id}
          >
            <Status status={r.status} dot />
            <span class="min-w-0 flex-1 truncate font-mono">{nameOf(r.id)}</span>
          </a>
        {/each}
      {/if}
    {/if}
  {/if}
{/each}
