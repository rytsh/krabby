<script>
  import Icon from "./Icon.svelte";

  // A hover/focus info icon. The bubble is position: fixed so it escapes the
  // overflow-hidden cards it usually sits in.
  let { label = "Details", tone = "", children } = $props();

  let anchor = $state(null);
  let pos = $state(null);

  function show() {
    const r = anchor.getBoundingClientRect();
    const below = r.bottom + 6;
    pos = { top: below, right: Math.max(8, window.innerWidth - r.right - 4) };
  }

  function hide() {
    pos = null;
  }
</script>

<span
  bind:this={anchor}
  class="inline-flex cursor-help items-center {tone === 'warn' ? 'text-warn' : 'text-faint hover:text-fg'}"
  role="button"
  tabindex="0"
  aria-label={label}
  onmouseenter={show}
  onmouseleave={hide}
  onfocus={show}
  onblur={hide}
  onclick={(e) => {
    e.preventDefault();
    e.stopPropagation();
  }}
  onkeydown={(e) => e.key === "Escape" && hide()}
>
  <Icon name="info" size={14} />
</span>

{#if pos}
  <div
    role="tooltip"
    class="pointer-events-none fixed z-50 max-w-xs break-words rounded-md border border-line-strong bg-surface-3 px-2.5 py-1.5 text-left text-[11.5px] leading-relaxed text-dim shadow-lg"
    style={`top: ${pos.top}px; right: ${pos.right}px`}
  >
    {@render children()}
  </div>
{/if}
