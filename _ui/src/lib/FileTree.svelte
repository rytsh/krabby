<script>
  import FileTree from './FileTree.svelte';
  // Recursive, lazily-loaded file tree. Directories expand in place; children
  // are fetched on first expand and cached by the parent in `children`.
  import Icon from "./Icon.svelte";
  import FileIcon from "./FileIcon.svelte";

  /**
   * @typedef {Object} Props
   * @property {any} [entries]
   * @property {number} [depth]
   * @property {any} [selected]
   * @property {any} [expanded]
   * @property {any} [children]
   * @property {any} onToggle
   * @property {any} onOpen
   */

  /** @type {Props} */
  let {
    entries = [],
    depth = 0,
    selected = null,
    expanded = {},
    children = {},
    onToggle,
    onOpen
  } = $props();

  function baseName(p) {
    return p.includes("/") ? p.slice(p.lastIndexOf("/") + 1) : p;
  }
</script>

<ul class="m-0 list-none p-0">
  {#each entries as e (e.path)}
    <li>
      <button
        class="flex w-full items-center gap-1.5 rounded-md py-1 pr-2 text-left text-[13px] text-dim hover:bg-surface-2 hover:text-fg"
        class:!bg-surface-2={selected === e.path}
        class:!text-fg={selected === e.path}
        style={`padding-left: ${8 + depth * 14}px`}
        title={e.path}
        onclick={() => (e.is_dir ? onToggle(e) : onOpen(e))}
      >
        {#if e.is_dir}
          <span class="flex-shrink-0 text-faint">
            <Icon name={expanded[e.path] ? "chevron-down" : "chevron-right"} size={12} />
          </span>
          <FileIcon name={baseName(e.path)} isDir expanded={!!expanded[e.path]} size={14} />
        {:else}
          <span class="w-[12px] flex-shrink-0"></span>
          <FileIcon name={baseName(e.path)} size={14} />
        {/if}
        <span class="truncate font-mono">{baseName(e.path)}</span>
      </button>

      {#if e.is_dir && expanded[e.path]}
        {#if !children[e.path]}
          <div class="py-1 text-[12px] text-faint" style={`padding-left: ${8 + (depth + 1) * 14 + 30}px`}>Loading…</div>
        {:else if children[e.path].length === 0}
          <div class="py-1 text-[12px] text-faint" style={`padding-left: ${8 + (depth + 1) * 14 + 30}px`}>empty</div>
        {:else}
          <FileTree
            entries={children[e.path]}
            depth={depth + 1}
            {selected}
            {expanded}
            {children}
            {onToggle}
            {onOpen}
          />
        {/if}
      {/if}
    </li>
  {/each}
</ul>
