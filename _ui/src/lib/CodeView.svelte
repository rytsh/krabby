<script>
  // Source viewer with Shiki syntax highlighting (https://shiki.style/).
  // Shiki is imported lazily so the main bundle stays small; grammars/themes
  // are code-split by Vite and fetched on demand. The plain <pre> renders
  // immediately and is swapped for the highlighted HTML once ready.
  import { tick } from "svelte";


  /**
   * @typedef {Object} Props
   * @property {string} [code]
   * @property {string} [path]
   * @property {number} [scrollTo] - 1-based line to highlight and scroll into view (0 = none).
   */

  /** @type {Props} */
  let { code = "", path = "", scrollTo = 0 } = $props();

  let container = $state();

  async function applyScrollTo() {
    if (!container) return;
    await tick();
    container.querySelectorAll(".line-target").forEach((n) => n.classList.remove("line-target"));
    if (!scrollTo) return;
    const el = container.querySelectorAll(".line")[scrollTo - 1];
    if (!el) return;
    el.classList.add("line-target");
    el.scrollIntoView({ block: "center" });
  }

  // Skip highlighting for very large files to keep the UI responsive.
  const MAX_HIGHLIGHT_CHARS = 300_000;

  // File extension (or exact filename) → Shiki language id.
  const extToLang = {
    go: "go",
    js: "javascript",
    mjs: "javascript",
    cjs: "javascript",
    jsx: "jsx",
    ts: "typescript",
    mts: "typescript",
    cts: "typescript",
    tsx: "tsx",
    svelte: "svelte",
    vue: "vue",
    py: "python",
    rb: "ruby",
    rs: "rust",
    c: "c",
    h: "c",
    cc: "cpp",
    cpp: "cpp",
    cxx: "cpp",
    hpp: "cpp",
    cs: "csharp",
    java: "java",
    kt: "kotlin",
    kts: "kotlin",
    swift: "swift",
    php: "php",
    scala: "scala",
    sh: "shellscript",
    bash: "shellscript",
    zsh: "shellscript",
    fish: "fish",
    ps1: "powershell",
    sql: "sql",
    html: "html",
    htm: "html",
    css: "css",
    scss: "scss",
    sass: "sass",
    less: "less",
    json: "json",
    jsonc: "jsonc",
    json5: "json5",
    yaml: "yaml",
    yml: "yaml",
    toml: "toml",
    ini: "ini",
    xml: "xml",
    svg: "xml",
    md: "markdown",
    markdown: "markdown",
    mdx: "mdx",
    graphql: "graphql",
    gql: "graphql",
    proto: "proto",
    tf: "terraform",
    hcl: "hcl",
    lua: "lua",
    r: "r",
    pl: "perl",
    ex: "elixir",
    exs: "elixir",
    erl: "erlang",
    hs: "haskell",
    ml: "ocaml",
    clj: "clojure",
    zig: "zig",
    d: "d",
    dart: "dart",
    groovy: "groovy",
    gradle: "groovy",
    cmake: "cmake",
    nix: "nix",
    diff: "diff",
    patch: "diff",
    tex: "latex",
    vim: "viml",
    dockerfile: "docker",
    makefile: "make",
    csv: "csv",
    prisma: "prisma",
    astro: "astro",
  };

  const nameToLang = {
    dockerfile: "docker",
    makefile: "make",
    gnumakefile: "make",
    "cmakelists.txt": "cmake",
    ".bashrc": "shellscript",
    ".zshrc": "shellscript",
    "go.mod": "go-module",
    "go.sum": "go-sum",
  };

  function langFor(p) {
    const base = (p.includes("/") ? p.slice(p.lastIndexOf("/") + 1) : p).toLowerCase();
    if (nameToLang[base]) return nameToLang[base];
    const dot = base.lastIndexOf(".");
    if (dot < 0) return "";
    return extToLang[base.slice(dot + 1)] || "";
  }

  let html = $state("");
  let seq = 0;

  // Plain-text fallback rendered line by line so the CSS line-number counter
  // applies to it exactly like Shiki's .line spans.
  let plainLines = $derived((() => {
    const lines = code.split("\n");
    if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();
    return lines;
  })());

  async function highlight(source, p) {
    const id = ++seq;
    html = "";
    const lang = langFor(p);
    if (!lang || !source || source.length > MAX_HIGHLIGHT_CHARS) return;
    try {
      const { codeToHtml, bundledLanguages } = await import("shiki");
      if (!(lang in bundledLanguages)) return;
      // Code is always rendered dark regardless of the app theme.
      const out = await codeToHtml(source, { lang, theme: "github-dark" });
      if (id === seq) html = out; // ignore stale results after switching files
    } catch {
      // Highlighting is best-effort; the plain view below already shows the code.
    }
  }

  $effect(() => {
    highlight(code, path);
  });

  // Re-apply the target line whenever content, highlight state or the
  // requested line changes.
  $effect(() => {
    code;
    html;
    scrollTo;
    applyScrollTo();
  });
</script>

<div bind:this={container}>
  {#if html}
    <div class="code-view overflow-x-auto p-3.5 font-mono text-[12.5px] leading-relaxed">
      {@html html}
    </div>
  {:else}
    <pre class="code-view m-0 overflow-x-auto p-3.5 font-mono text-[12.5px] leading-relaxed"><code>{#each plainLines as l}<span class="line">{l}</span>{"\n"}{/each}</code></pre>
  {/if}
</div>
