<script>
  import { AlertTriangle, X } from "@lucide/svelte";
  import { dismissToast, toasts } from "./toast.js";
</script>

<div
  class="pointer-events-none fixed bottom-4 right-4 z-[100] flex w-[min(24rem,calc(100vw-2rem))] flex-col gap-2"
  aria-live="assertive"
  aria-atomic="false"
>
  {#each $toasts as toast (toast.id)}
    <div class="pointer-events-auto flex items-start gap-3 rounded-lg border border-err/40 bg-surface px-3.5 py-3 text-[13px] shadow-lg" role="alert">
      <AlertTriangle class="mt-0.5 flex-shrink-0 text-err" size={17} strokeWidth={1.8} aria-hidden="true" />
      <span class="min-w-0 flex-1 break-words leading-5 text-fg">{toast.message}</span>
      <button
        class="-mr-1 -mt-1 inline-flex h-7 w-7 flex-shrink-0 cursor-pointer items-center justify-center rounded text-faint transition-colors hover:bg-surface-2 hover:text-fg"
        aria-label="Dismiss notification"
        onclick={() => dismissToast(toast.id)}
      >
        <X size={15} strokeWidth={1.8} aria-hidden="true" />
      </button>
    </div>
  {/each}
</div>
