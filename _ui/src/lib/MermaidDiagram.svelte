<script>
  // GitHub-style mermaid viewer: the rendered SVG sits in a fixed-height
  // viewport with drag-to-pan and hover zoom controls (svg-pan-zoom).
  import Icon from "./Icon.svelte";

  /** @type {{ svg?: string }} */
  let { svg = "" } = $props();

  let root = $state();
  let host = $state();
  let instance = null;
  let fullscreen = $state(false);

  function resetView() {
    if (!instance) return;
    instance.resize();
    instance.fit();
    instance.center();
  }

  async function toggleFullscreen() {
    if (!root) return;
    if (document.fullscreenElement === root) {
      await document.exitFullscreen();
    } else {
      await root.requestFullscreen();
    }
  }

  $effect(() => {
    if (!host) return;
    host.innerHTML = svg;
    const el = host.querySelector("svg");
    if (!el) return;

    // Viewport height follows the diagram's natural size within bounds.
    const natural =
      (el.viewBox && el.viewBox.baseVal && el.viewBox.baseVal.height) ||
      el.getBoundingClientRect().height ||
      320;
    host.style.height = `${Math.max(180, Math.min(Math.ceil(natural) + 40, 520))}px`;

    el.style.width = "100%";
    el.style.height = "100%";
    el.style.maxWidth = "none";

    let cancelled = false;
    import("svg-pan-zoom")
      .then(({ default: svgPanZoom }) => {
        if (cancelled || !el.isConnected) return;
        instance = svgPanZoom(el, {
          fit: true,
          center: true,
          minZoom: 0.3,
          maxZoom: 12,
          zoomScaleSensitivity: 0.35,
          // Wheel zoom would hijack page scrolling inside the docs pane.
          mouseWheelZoomEnabled: false,
          dblClickZoomEnabled: true,
          controlIconsEnabled: false,
        });
      })
      .catch((err) => {
        console.warn("[krabby] mermaid zoom controls failed to load", err);
      });

    return () => {
      cancelled = true;
      if (instance) {
        instance.destroy();
        instance = null;
      }
    };
  });

  $effect(() => {
    const handleFullscreen = () => {
      fullscreen = document.fullscreenElement === root;
      requestAnimationFrame(resetView);
    };
    document.addEventListener("fullscreenchange", handleFullscreen);
    return () => document.removeEventListener("fullscreenchange", handleFullscreen);
  });
</script>

<div
  bind:this={root}
  class="mermaid-diagram relative my-3 overflow-hidden rounded-lg border border-line bg-surface-2"
>
  <div bind:this={host} class="mermaid-viewport w-full cursor-grab active:cursor-grabbing"></div>

  <div
    class="absolute right-2 top-2 flex flex-col overflow-hidden rounded-md border border-line bg-surface shadow"
    aria-label="Diagram controls"
  >
    <button class="mmd-btn" title="Zoom in" aria-label="Zoom in" onclick={() => instance?.zoomIn()}>
      <Icon name="plus" size={14} />
    </button>
    <button class="mmd-btn" title="Zoom out" aria-label="Zoom out" onclick={() => instance?.zoomOut()}>
      <Icon name="minus" size={14} />
    </button>
    <button class="mmd-btn" title="Reset view" aria-label="Reset view" onclick={resetView}>
      <Icon name="reset" size={14} />
    </button>
    <button
      class="mmd-btn"
      title={fullscreen ? "Exit full screen" : "View full screen"}
      aria-label={fullscreen ? "Exit full screen" : "View full screen"}
      onclick={toggleFullscreen}
    >
      <Icon name={fullscreen ? "minimize" : "maximize"} size={14} />
    </button>
  </div>
</div>

<style>
  .mmd-btn {
    display: flex;
    width: 2rem;
    height: 2rem;
    cursor: pointer;
    align-items: center;
    justify-content: center;
    border: 0;
    background: var(--surface);
    color: var(--dim);
  }

  .mmd-btn + .mmd-btn {
    border-top: 1px solid var(--line);
  }

  .mmd-btn:hover,
  .mmd-btn:focus-visible {
    background: var(--line);
    color: var(--fg);
    outline: none;
  }

  .mermaid-diagram:fullscreen {
    width: 100%;
    height: 100%;
    margin: 0;
    border: 0;
    border-radius: 0;
    background: var(--surface-2);
  }

  .mermaid-diagram:fullscreen .mermaid-viewport {
    height: 100% !important;
  }

  :global(.mermaid-viewport svg) {
    display: block;
    max-width: none;
  }
</style>
