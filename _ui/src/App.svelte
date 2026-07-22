<script>
  import { onMount } from "svelte";
  import { path, link } from "./lib/router.js";
  import { theme, toggleTheme } from "./lib/theme.js";
  import { repoGroups, loadRepos } from "./lib/repos.js";
  import Icon from "./lib/Icon.svelte";
  import BrandIcon from "./lib/BrandIcon.svelte";
  import Status from "./lib/Status.svelte";
  import ToastHost from "./lib/ToastHost.svelte";
  import Repos from "./routes/Repos.svelte";
  import RepoDetail from "./routes/RepoDetail.svelte";
  import Activity from "./routes/Activity.svelte";
  import Search from "./routes/Search.svelte";
  import Settings from "./routes/Settings.svelte";
  import About from "./routes/About.svelte";

  // Resolve the current route from the pathname (query string stripped). Repo
  // ids are owner/name, so /repos/<owner>/<name> maps to the detail view.
  let route = $derived.by(() => {
    const p = $path.split("?")[0].replace(/\/$/, "") || "/";
    if (p === "/" || p === "/repos") return { view: "repos" };
    if (p.startsWith("/repos/")) return { view: "repo", repoId: p.slice("/repos/".length) };
    if (p === "/search") return { view: "search" };
    if (p === "/activity") return { view: "activity" };
    if (p === "/settings") return { view: "settings" };
    if (p === "/about") return { view: "about" };
    return { view: "repos" };
  });
  let view = $derived(route.view);
  let repoId = $derived(route.repoId);

  const nav = [
    { href: "/repos", label: "Repositories", icon: "boxes", match: (v) => v === "repos" || v === "repo" },
    { href: "/activity", label: "Activity", icon: "activity", match: (v) => v === "activity" },
    { href: "/search", label: "Code search", icon: "search", match: (v) => v === "search" },
    { href: "/settings", label: "Settings", icon: "settings", match: (v) => v === "settings" },
    { href: "/about", label: "About", icon: "book", match: (v) => v === "about" },
  ];

  const title = {
    repos: "Repositories",
    repo: "Repository",
    activity: "Activity",
    search: "Code search",
    settings: "Settings",
    about: "About",
  };

  // Collapsed state per owner group, persisted so it survives reloads.
  const COLLAPSE_KEY = "krabby-sidebar-collapsed";
  let collapsed = $state({});
  try {
    collapsed = JSON.parse(localStorage.getItem(COLLAPSE_KEY) || "{}") || {};
  } catch {
    collapsed = {};
  }

  function toggleGroup(owner) {
    collapsed = { ...collapsed, [owner]: !collapsed[owner] };
    localStorage.setItem(COLLAPSE_KEY, JSON.stringify(collapsed));
  }

  function repoName(id) {
    const idx = id.indexOf("/");
    return idx > 0 ? id.slice(idx + 1) : id;
  }

  const SIDEBAR_WIDTH_KEY = "krabby-sidebar-width";
  const savedSidebarW = Number(localStorage.getItem(SIDEBAR_WIDTH_KEY));
  let sidebarW = $state(
    Number.isFinite(savedSidebarW) && savedSidebarW > 0 ? Math.min(420, Math.max(180, savedSidebarW)) : 240,
  );

  const SIDEBAR_OPEN_KEY = "krabby-sidebar-open";
  let sidebarOpen = $state(localStorage.getItem(SIDEBAR_OPEN_KEY) !== "0");

  function toggleSidebar() {
    sidebarOpen = !sidebarOpen;
    localStorage.setItem(SIDEBAR_OPEN_KEY, sidebarOpen ? "1" : "0");
  }

  function startSidebarDrag(e) {
    e.preventDefault();
    const startX = e.clientX;
    const startW = sidebarW;

    function move(ev) {
      sidebarW = Math.min(420, Math.max(180, startW + ev.clientX - startX));
    }

    function up() {
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      localStorage.setItem(SIDEBAR_WIDTH_KEY, String(sidebarW));
    }

    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
  }

  // Load once for the global sidebar. The Activity page owns optional live
  // polling so background requests are not made unless the user enables it.
  onMount(loadRepos);
</script>

<div class="flex min-h-screen">
  {#if sidebarOpen}
    <aside class="sticky top-0 flex h-screen flex-shrink-0 flex-col overflow-y-auto bg-surface p-3" style={`width:${sidebarW}px`}>
    <div class="flex items-center gap-2 px-2 pb-5 pt-2">
      <span class="grid h-7 w-7 place-items-center rounded-md bg-accent text-accent-fg">
        <Icon name="warehouse" size={16} />
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

    {#if $repoGroups.length > 0}
      <div class="mt-5 px-2.5 pb-1.5 text-[11px] font-medium uppercase tracking-wider text-faint">Repositories</div>
      <nav class="flex flex-col gap-0.5">
        {#each $repoGroups as group (group.owner)}
          <button
            class="flex w-full cursor-pointer items-center gap-1.5 rounded-md px-2.5 py-1.5 text-left text-[13px] text-dim transition-colors hover:bg-surface-2 hover:text-fg"
            onclick={() => toggleGroup(group.owner)}
            aria-expanded={!collapsed[group.owner]}
          >
            <Icon name={collapsed[group.owner] ? "chevron-right" : "chevron-down"} size={13} />
            <Icon name="folder" size={13} />
            <span class="truncate font-mono">{group.owner || "(root)"}</span>
            <span class="ml-auto text-[11px] text-faint">{group.repos.length}</span>
          </button>
          {#if !collapsed[group.owner]}
            {#each group.repos as r (r.id)}
              <a
                href={`/repos/${r.id}`}
                use:link
                title={r.id}
                class="flex items-center gap-2 rounded-md py-1.5 pl-[34px] pr-2.5 text-[13px] text-dim transition-colors hover:bg-surface-2 hover:text-fg"
                class:!bg-surface-2={view === "repo" && repoId === r.id}
                class:!text-fg={view === "repo" && repoId === r.id}
              >
                <Status status={r.status} dot />
                <span class="truncate font-mono">{repoName(r.id)}</span>
              </a>
            {/each}
          {/if}
        {/each}
      </nav>
    {/if}

    <div class="mt-auto px-2 pb-1 pt-4 text-xs text-faint">multi-repo graphify knowledge graphs</div>
  </aside>

  <div
    class="sticky top-0 z-20 h-screen w-[3px] flex-shrink-0 cursor-col-resize bg-line transition-colors hover:bg-accent"
    role="separator"
    aria-label="Resize navigation sidebar"
    aria-orientation="vertical"
    aria-valuemin="180"
    aria-valuemax="420"
    aria-valuenow={sidebarW}
    onpointerdown={startSidebarDrag}
  ></div>
  {/if}

  <div class="flex min-w-0 flex-1 flex-col">
    <header class="sticky top-0 z-10 flex items-center justify-between border-b border-line bg-bg/80 px-2 py-1 backdrop-blur">
      <div class="flex min-w-0 items-center gap-2">
        <button
          class="icon-btn"
          onclick={toggleSidebar}
          title={sidebarOpen ? "Hide sidebar" : "Show sidebar"}
          aria-label={sidebarOpen ? "Hide sidebar" : "Show sidebar"}
        >
          <Icon name={sidebarOpen ? "panel-left-close" : "panel-left-open"} />
        </button>
        {#if view === "repo"}
          <div class="flex min-w-0 items-center gap-2 text-[15px]">
            <a href="/repos" use:link class="text-dim transition-colors hover:text-fg">Repositories</a>
            <span class="text-faint">/</span>
            <span class="truncate font-mono font-semibold">{repoId}</span>
          </div>
        {:else}
          <div class="flex min-w-0 items-baseline gap-3">
            <h1 class="shrink-0 text-[15px] font-semibold">{title[view] || "krabby"}</h1>
            {#if view === "search"}
              <span class="truncate text-[11px] text-faint">source code and generated documentation search</span>
            {/if}
          </div>
        {/if}
      </div>

      <div class="flex items-center gap-2">
        <button
          class="icon-btn"
          onclick={toggleTheme}
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
          <BrandIcon name="github" />
        </a>
      </div>
    </header>

    <main class="min-w-0 flex-1 px-2 {view === 'repo' ? 'py-2' : 'max-w-[1280px] py-7'}">
      {#if view === "repos"}
        <Repos />
      {:else if view === "repo"}
        {#key repoId}
          <RepoDetail {repoId} />
        {/key}
      {:else if view === "activity"}
        <Activity />
      {:else if view === "search"}
        <Search />
      {:else if view === "settings"}
        <Settings />
      {:else if view === "about"}
        <About />
      {/if}
    </main>
  </div>
</div>

<ToastHost />
