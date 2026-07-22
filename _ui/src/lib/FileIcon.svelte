<script module>
  // Colored file-type icons via Iconify's vscode-icons set — everything comes
  // from npm, no asset copying. The icon data JSON is lazy-loaded once and
  // registered offline so no network requests are made at render time.
  import IconifyIcon, { addCollection, iconLoaded } from "@iconify/svelte";
  import { getIconForFile, getIconForFolder, getIconForOpenFolder } from "vscode-icons-js";
  import { writable } from "svelte/store";

  const ready = writable(false);
  let loading = false;

  function ensureLoaded() {
    if (loading) return;
    loading = true;
    import("@iconify-json/vscode-icons/icons.json")
      .then((m) => {
        addCollection(m.default ?? m);
        ready.set(true);
      })
      .catch(() => {
        loading = false; // allow a retry on the next mount
      });
  }

  // vscode-icons-js returns svg filenames like "file_type_go.svg"; the Iconify
  
  function toIconifyName(svgName) {
    return "vscode-icons:" + svgName.replace(/\.svg$/, "").replace(/_/g, "-");
  }
</script>

<script>
  /**
   * @typedef {Object} Props
   * @property {string} [name] - set uses "vscode-icons:file-type-go". - base file or directory name
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

  let icon = $derived((() => {
    if (!$ready) return "";
    const svg = isDir
      ? (expanded ? getIconForOpenFolder(name) : getIconForFolder(name))
      : getIconForFile(name);
    let n = toIconifyName(svg || (isDir ? "default_folder.svg" : "default_file.svg"));
    if (!iconLoaded(n)) {
      n = isDir
        ? expanded
          ? "vscode-icons:default-folder-opened"
          : "vscode-icons:default-folder"
        : "vscode-icons:default-file";
    }
    return n;
  })());
</script>

{#if icon}
  <IconifyIcon {icon} width={size} height={size} class="flex-shrink-0" />
{:else}
  <span class="inline-block flex-shrink-0" style={`width:${size}px;height:${size}px`}></span>
{/if}
