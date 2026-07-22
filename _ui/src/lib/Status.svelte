<script>

  /**
   * @typedef {Object} Props
   * @property {string} [status]
   * @property {boolean} [dot] - When true only the colored dot is rendered (compact, e.g. sidebar).
   */

  /** @type {Props} */
  let { status = "", dot = false } = $props();

  const colors = {
    ready: "var(--color-ok)",
    building: "var(--color-busy)",
    cloning: "var(--color-busy)",
    fetching: "var(--color-busy)",
    pending: "var(--color-warn)",
    error: "var(--color-err)",
  };
</script>

{#if dot}
  <span
    class="inline-block h-[7px] w-[7px] flex-shrink-0 rounded-[1px]"
    title={status || "unknown"}
    style="background: {colors[status] || 'var(--color-faint)'}"
  ></span>
{:else}
  <span class="inline-flex items-center gap-1.5 text-[13px] text-dim">
    <span
      class="inline-block h-[7px] w-[7px] rounded-full"
      style="background: {colors[status] || 'var(--color-faint)'}"
    ></span>
    {status || "unknown"}
  </span>
{/if}
