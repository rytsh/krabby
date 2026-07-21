<script>
  export let status = "";

  const colors = {
    ready: "var(--color-ok)",
    building: "var(--color-busy)",
    cloning: "var(--color-busy)",
    pending: "var(--color-warn)",
    error: "var(--color-err)",
  };
</script>

<span class="inline-flex items-center gap-1.5 text-[13px] text-dim">
  <span
    class="inline-block h-[7px] w-[7px] rounded-full"
    style="background: {colors[status] || 'var(--color-faint)'}"
  ></span>
  {status || "unknown"}
</span>
