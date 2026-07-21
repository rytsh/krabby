<script>
  import { path, link } from "./lib/router.js";
  import { theme, toggleTheme } from "./lib/theme.js";
  import Icon from "./lib/Icon.svelte";
  import Repos from "./routes/Repos.svelte";
  import RepoDetail from "./routes/RepoDetail.svelte";
  import Settings from "./routes/Settings.svelte";

  // Resolve the current route from the pathname. Repo ids are owner/name, so
  // /repos/<owner>/<name> maps to the detail view.
  let view, repoId;
  $: {
    const p = $path.replace(/\/$/, "") || "/";
    if (p === "/" || p === "/repos") {
      view = "repos";
    } else if (p.startsWith("/repos/")) {
      view = "repo";
      repoId = p.slice("/repos/".length);
    } else if (p === "/settings") {
      view = "settings";
    } else {
      view = "repos";
    }
  }

  const nav = [
    { href: "/repos", label: "Repositories", icon: "graph", match: (v) => v === "repos" || v === "repo" },
    { href: "/settings", label: "Settings", icon: "settings", match: (v) => v === "settings" },
  ];

  const title = { repos: "Repositories", repo: "Repository", settings: "Settings" };
</script>

<div class="flex min-h-screen">
  <aside class="flex w-60 flex-shrink-0 flex-col border-r border-line bg-surface p-3">
    <div class="flex items-center gap-2 px-2 pb-5 pt-2">
      <span class="grid h-7 w-7 place-items-center rounded-md bg-accent text-accent-fg">
        <Icon name="graph" size={16} />
      </span>
      <span class="text-base font-semibold tracking-tight">krabby</span>
    </div>

    <nav class="flex flex-col gap-0.5">
      {#each nav as item}
        <a
          href={item.href}
          use:link
          class="flex items-center gap-2.5 rounded-md px-2.5 py-2 text-sm text-dim transition-colors hover:bg-surface-2 hover:text-fg"
          class:!bg-surface-2={item.match(view)}
          class:!text-fg={item.match(view)}
        >
          <Icon name={item.icon} size={16} />
          {item.label}
        </a>
      {/each}
    </nav>

    <div class="mt-auto px-2 pb-1 text-xs text-faint">multi-repo graphify knowledge graphs</div>
  </aside>

  <div class="flex min-w-0 flex-1 flex-col">
    <header class="sticky top-0 z-10 flex h-14 items-center justify-between border-b border-line bg-bg/80 px-8 backdrop-blur">
      <h1 class="text-[15px] font-semibold">{title[view] || "krabby"}</h1>

      <div class="flex items-center gap-2">
        <button
          class="icon-btn"
          on:click={toggleTheme}
          title={$theme === "dark" ? "Switch to light mode" : "Switch to dark mode"}
          aria-label="Toggle color theme"
        >
          <Icon name={$theme === "dark" ? "sun" : "moon"} />
        </button>
        <a
          class="icon-btn"
          href="https://github.com/rytsh/krabby"
          target="_blank"
          rel="noreferrer noopener"
          title="View krabby on GitHub"
          aria-label="GitHub repository"
        >
          <Icon name="github" />
        </a>
      </div>
    </header>

    <main class="min-w-0 flex-1 max-w-[1100px] px-8 py-7">
      {#if view === "repos"}
        <Repos />
      {:else if view === "repo"}
        <RepoDetail {repoId} />
      {:else if view === "settings"}
        <Settings />
      {/if}
    </main>
  </div>
</div>
