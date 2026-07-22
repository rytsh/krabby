<script>
  // Markdown renderer: comark (https://comark.dev/) renders CommonMark + GFM to
  // an HTML string (pipe tables, strikethrough, task lists) and gives each
  // heading a stable id. Fenced ```mermaid blocks are upgraded to interactive
  // SVG after the HTML lands in the DOM. Both comark and mermaid are imported
  // lazily so they stay out of the main bundle.
  import { mount, onDestroy, tick, unmount } from "svelte";
  import MermaidDiagram from "./MermaidDiagram.svelte";
  import { theme } from "./theme.js";

  /**
   * @typedef {{ id: string, text: string, level: number }} Heading
   * @typedef {{ markdown?: string, onHeadings?: (headings: Heading[]) => void }} Props
   */

  /** @type {Props} */
  let { markdown = "", onHeadings = () => {} } = $props();

  // Rendered HTML is the single reactive source of truth for the view. Heading
  // ids come straight from comark's output, so we never mutate the DOM to build
  // the table of contents — that keeps the post-render effect side-effect free
  // on the reactive graph and avoids infinite effect loops.
  let html = $state("");

  // Plain DOM handle (NOT $state) so the mermaid effect can read it without
  // taking a reactive dependency on it.
  /** @type {HTMLElement | undefined} */
  let container;

  // Monotonic token: every new render invalidates in-flight async work from the
  // previous markdown/theme so stale diagrams never mount into fresh HTML.
  let seq = 0;
  let mermaidId = 0;
  /** @type {ReturnType<typeof mount>[]} */
  let mountedDiagrams = [];

  function clearDiagrams() {
    for (const diagram of mountedDiagrams) unmount(diagram);
    mountedDiagrams = [];
  }

  // Extract the heading outline from comark's HTML string without touching the
  // live DOM. comark already emitted an id on every heading.
  /** @param {string} source */
  function extractHeadings(source) {
    const doc = new DOMParser().parseFromString(source, "text/html");
    /** @type {Heading[]} */
    const headings = [];
    for (const el of doc.querySelectorAll("h1, h2, h3")) {
      headings.push({
        id: el.id,
        text: (el.textContent || "").trim(),
        level: Number(el.tagName.slice(1)),
      });
    }
    return headings;
  }

  /** @param {string} src */
  async function render(src) {
    const id = ++seq;
    if (!src) {
      clearDiagrams();
      html = "";
      onHeadings([]);
      return;
    }
    try {
      const { render: renderMarkdown } = await import("@comark/html");
      const out = await renderMarkdown(src);
      if (id !== seq) return;
      clearDiagrams();
      html = out;
      onHeadings(extractHeadings(out));
    } catch (err) {
      if (id !== seq) return;
      console.warn("[krabby] markdown render failed", err);
      html = "";
      onHeadings([]);
    }
  }

  // Re-render when the markdown or the app theme changes (theme drives the
  // mermaid palette). This effect only writes `html`, which the mermaid effect
  // below reacts to — no cycle.
  $effect(() => {
    void $theme;
    render(markdown);
  });

  // Turn fenced mermaid code blocks into rendered SVG once fresh HTML is in the
  // DOM. Depends solely on `html`; it reads `container` as a plain DOM handle
  // and never writes reactive state, so it cannot re-trigger itself.
  $effect(() => {
    void html;
    const renderSeq = seq;
    tick().then(() => renderMermaid(renderSeq));
  });

  /** @param {number} renderSeq */
  async function renderMermaid(renderSeq) {
    if (renderSeq !== seq || !container) return;
    const blocks = container.querySelectorAll("pre > code.language-mermaid");
    if (blocks.length === 0) return;
    try {
      const { default: mermaid } = await import("mermaid");
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        theme: $theme === "dark" ? "dark" : "default",
        // Never inject mermaid's big "Syntax error" bomb diagram into the DOM.
        suppressErrorRendering: true,
      });
      for (const code of blocks) {
        if (renderSeq !== seq) return;
        const pre = code.closest("pre");
        if (!pre || !pre.isConnected) continue;
        // LLMs often emit escaped quotes (\") inside quoted node labels, which
        // mermaid cannot parse — it expects the #quot; entity instead.
        const src = (code.textContent || "").replaceAll('\\"', "#quot;");
        const id = `mmd-${++mermaidId}`;
        try {
          // Validate first; invalid diagrams stay visible as plain code.
          try {
            await mermaid.parse(src);
          } catch (perr) {
            console.warn(
              `[krabby] invalid mermaid diagram skipped: ${perr?.message || perr}\n--- diagram source ---\n${src}`,
            );
            continue;
          }
          const { svg } = await mermaid.render(id, src);
          if (renderSeq !== seq || !pre.isConnected) continue;

          const target = document.createElement("div");
          pre.replaceWith(target);
          try {
            mountedDiagrams.push(mount(MermaidDiagram, { target, props: { svg } }));
          } catch (mountErr) {
            target.replaceWith(pre);
            throw mountErr;
          }
        } catch (rerr) {
          // Invalid mermaid syntax: keep the plain code block visible, and drop
          // the temp element mermaid may have left in <body>.
          console.warn(
            `[krabby] mermaid render failed: ${rerr?.message || rerr}\n--- diagram source ---\n${src}`,
          );
          document.getElementById(`d${id}`)?.remove();
        }
      }
    } catch (lerr) {
      // mermaid failed to load; code blocks stay as-is.
      console.warn("[krabby] mermaid failed to load", lerr);
    }
  }

  onDestroy(() => {
    seq++;
    clearDiagrams();
    onHeadings([]);
  });
</script>

<div bind:this={container} class="md-view p-5 text-[14px] leading-relaxed">
  {@html html}
</div>
