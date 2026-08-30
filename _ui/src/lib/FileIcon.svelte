<script module>
  import { addCollection } from "@iconify/svelte";
  import { writable } from "svelte/store";

  const ready = writable(false);
  let loading;

  function ensureLoaded() {
    if (loading) return;
    loading = import("./file-icons.generated.js")
      .then(({ vscodeIcons }) => {
        addCollection(vscodeIcons);
        ready.set(true);
      })
      .catch(() => {
        loading = undefined;
      });
  }
</script>

<script>
  import IconifyIcon from "@iconify/svelte";

  import { resolveFileIcon } from "./file-icons.js";

  /**
   * @typedef {Object} Props
   * @property {string} [name] - base file or directory name
   * @property {boolean} [isDir]
   * @property {boolean} [expanded]
   * @property {number} [size]
   */

  /** @type {Props} */
  let {
    name = "",
    isDir = false,
    expanded = false,
    size = 14
  } = $props();

  ensureLoaded();

  let icon = $derived($ready ? resolveFileIcon(name, { isDir, expanded }) : "");
</script>

{#if icon}
  <IconifyIcon {icon} width={size} height={size} class="flex-shrink-0" />
{:else}
  <span class="inline-block flex-shrink-0" style={`width:${size}px;height:${size}px`}></span>
{/if}
