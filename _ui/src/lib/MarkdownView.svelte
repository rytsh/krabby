<script>
  // Markdown renderer: comark (https://comark.dev/) for HTML plus mermaid for
  // fenced ```mermaid diagrams. comark is CommonMark + GFM, so pipe tables,
  // strikethrough and task lists render (unlike plain commonmark, which dropped
  // GFM tables to plain text). Both libraries are imported lazily so they stay
  // out of the main bundle. Raw inline/block HTML is disabled (`html: false`),
  // so any HTML in the markdown is escaped to text rather than injected — the
  // same XSS guard commonmark's safe mode gave us.
  import { mount, onDestroy, tick, unmount } from "svelte";
  import MermaidDiagram from "./MermaidDiagram.svelte";
  import { theme } from "./theme.js";

  /**
   * @typedef {{ id: string, text: string, level: number }} Heading
   * @typedef {{ markdown?: string, onHeadings?: (headings: Heading[]) => void }} Props
   */

  /** @type {Props} */
  let { markdown = "", onHeadings = () => {} } = $props();

  let html = $state("");
  let container = $state();
  let seq = 0;
  let mermaidId = 0;
  let mountedDiagrams = [];

  function clearDiagrams() {
    for (const diagram of mountedDiagrams) unmount(diagram);
    mountedDiagrams = [];
  }

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
      // html:false escapes raw HTML instead of injecting it (XSS guard).
      const out = await renderMarkdown(src, { html: false });
      if (id === seq) {
        clearDiagrams();
        html = out;
      }
    } catch {
      if (id === seq) html = "";
    }
  }

  $effect(() => {
    // Re-render diagrams when the Mermaid theme changes with the app theme.
    $theme;
    render(markdown);
  });

  function collectHeadings() {
    if (!container) return;

    const seen = new Map();
    const headings = Array.from(container.querySelectorAll("h1, h2, h3")).map((heading, index) => {
      const text = (heading.textContent || "").trim();
      const base =
        text
          .normalize("NFKD")
          .replace(/[\u0300-\u036f]/g, "")
          .toLowerCase()
          .replace(/[^a-z0-9]+/g, "-")
          .replace(/^-|-$/g, "") || `section-${index + 1}`;
      const occurrence = (seen.get(base) || 0) + 1;
      seen.set(base, occurrence);
      const id = `doc-${base}${occurrence > 1 ? `-${occurrence}` : ""}`;
      heading.id = id;

      return { id, text, level: Number(heading.tagName.slice(1)) };
    });

    onHeadings(headings);
  }

  // Replace mermaid code blocks with rendered SVG once the HTML is in the DOM.
  async function renderMermaid() {
    if (!container) return;
    const renderSeq = seq;
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
          // Invalid mermaid syntax: keep the plain code block visible, and
          // drop the temp element mermaid may have left in <body>.
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

  // Runs after the DOM has been updated with fresh HTML.
  $effect(() => {
    html;
    container;
    tick().then(() => {
      collectHeadings();
      renderMermaid();
    });
  });

  onDestroy(() => {
    clearDiagrams();
    onHeadings([]);
  });
</script>

<div bind:this={container} class="md-view p-5 text-[14px] leading-relaxed">
  {@html html}
</div>
