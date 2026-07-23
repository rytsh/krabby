<script>
  import { onMount, untrack } from "svelte";
  import { path, link } from "./lib/router.js";
  import { theme, toggleTheme } from "./lib/theme.js";
  import {
    owners,
    loadOwners,
    loadOwnerRepos,
    ownerOf,
  } from "./lib/repos.js";
  import { buildOwnerTree, collapseTree, sidebarPathMode } from "./lib/paths.js";
  import { api } from "./lib/api.js";
  import Icon from "./lib/Icon.svelte";
  import BrandIcon from "./lib/BrandIcon.svelte";
  import ToastHost from "./lib/ToastHost.svelte";
  import RepoTree from "./lib/RepoTree.svelte";
  import Repos from "./routes/Repos.svelte";
  import RepoDetail from "./routes/RepoDetail.svelte";
  import Sources from "./routes/Sources.svelte";
  import Namespaces from "./routes/Namespaces.svelte";
  import Activity from "./routes/Activity.svelte";
  import Search from "./routes/Search.svelte";
  import Settings from "./routes/Settings.svelte";
  import About from "./routes/About.svelte";

  // Resolve the current route from the pathname (query string stripped). Repo
  // ids are full paths (host/group/.../name) with any number of "/" segments,
  // so everything after /repos/ maps to the detail view.
  let route = $derived.by(() => {
    const p = $path.split("?")[0].replace(/\/$/, "") || "/";
    if (p === "/" || p === "/repos") return { view: "repos" };
    if (p.startsWith("/repos/")) return { view: "repo", repoId: p.slice("/repos/".length) };
    if (p === "/sources") return { view: "sources" };
    if (p.startsWith("/sources/")) return { view: "sources", sourceName: p.slice("/sources/".length) };
    if (p === "/namespaces") return { view: "namespaces" };
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
    { href: "/sources", label: "Sources", icon: "book", match: (v) => v === "sources" },
    { href: "/namespaces", label: "Namespaces", icon: "tag", match: (v) => v === "namespaces" },
    { href: "/activity", label: "Activity", icon: "activity", match: (v) => v === "activity" },
    { href: "/search", label: "Code search", icon: "search", match: (v) => v === "search" },
    { href: "/settings", label: "Settings", icon: "settings", match: (v) => v === "settings" },
    { href: "/about", label: "About", icon: "book", match: (v) => v === "about" },
  ];

  const title = {
    repos: "Repositories",
    repo: "Repository",
    sources: "Sources",
    namespaces: "Namespaces",
    activity: "Activity",
    search: "Code search",
    settings: "Settings",
    about: "About",
  };

  // Expanded state per owner group, persisted so it survives reloads. Groups
  // default to collapsed: a group's repos are fetched lazily only when it is
  // expanded, so the sidebar stays cheap with many owners.
  const EXPANDED_KEY = "krabby-sidebar-expanded";
  let expanded = $state({});
  try {
    expanded = JSON.parse(localStorage.getItem(EXPANDED_KEY) || "{}") || {};
  } catch {
    expanded = {};
  }

  function persistExpanded() {
    localStorage.setItem(EXPANDED_KEY, JSON.stringify(expanded));
  }

  // Nested folder tree built from the flat owner list, so that groups like
  // ".../parser" and ".../parser/poc" nest as parent/child. In "full" mode the
  // long shared prefix chain (host/org/team/...) is kept as separate levels; in
  // "smart" mode single-child prefix chains are collapsed into one row.
  let ownerTree = $derived.by(() => {
    const tree = buildOwnerTree($owners);
    return $sidebarPathMode === "full" ? tree : collapseTree(tree);
  });

  // A tree node is expandable by its full path key; when the node is also a real
  // owner group (node.owner != null) its repos are loaded lazily on expand.
  function toggleNode(node) {
    const next = !expanded[node.key];
    expanded = { ...expanded, [node.key]: next };
    persistExpanded();
    if (next && node.owner !== null) loadOwnerRepos(node.owner);
  }

  // Expand every ancestor folder of an owner path (its own key included) so a
  // deeply nested active repo is revealed in the sidebar. Keys are the running
  // "/"-joined prefixes of the owner path.
  function expandAncestors(owner) {
    const segs = owner === "" ? [""] : owner.split("/");
    let path = "";
    const next = { ...expanded };
    let changed = false;
    for (const seg of segs) {
      path = path ? `${path}/${seg}` : seg;
      if (!next[path]) {
        next[path] = true;
        changed = true;
      }
    }
    // Only write when something actually changed. Reassigning `expanded` with a
    // fresh object every time would retrigger the ancestor effect (which reads
    // `expanded`) forever — Svelte 5 effect_update_depth_exceeded.
    if (!changed) return;
    expanded = next;
    persistExpanded();
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

  // Load the owner list once for the sidebar tree. Each owner's repos are
  // fetched lazily when its group is expanded (see toggleGroup/expandGroup).
  onMount(loadOwners);

  // Build metadata (version / commit / date) for the sidebar footer, so the
  // running build is identifiable from every page.
  let build = $state(null);
  onMount(async () => {
    try {
      build = await api.settings();
    } catch {
      build = null;
    }
  });
  let buildDate = $derived.by(() => {
    if (!build || !build.build_date || build.build_date === "-") return "";
    const d = new Date(build.build_date);
    return Number.isNaN(d.getTime()) ? build.build_date : d.toLocaleString();
  });

  // When viewing a repo, make sure its owner group is expanded and loaded so
  // the active repo is visible and highlighted in the sidebar.
  $effect(() => {
    if (view === "repo" && repoId) {
      const owner = ownerOf(repoId);
      // Reveal the repo when navigation changes, but do not make manual
      // collapse state a dependency that immediately reopens the group.
      untrack(() => expandAncestors(owner));
      loadOwnerRepos(owner);
    }
  });
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

    {#if $owners.length > 0}
      <div class="mt-5 px-2.5 pb-1.5 text-[11px] font-medium uppercase tracking-wider text-faint">Repositories</div>
      <nav class="flex flex-col gap-0.5">
        <RepoTree nodes={ownerTree} depth={0} {expanded} onToggle={toggleNode} {view} {repoId} />
      </nav>
    {/if}

    <div class="mt-auto px-2 pb-1 pt-4 text-xs text-faint">
      <div>multi-repo graphify knowledge graphs</div>
      {#if build}
        <div
          class="mt-2 font-mono text-[10px] leading-relaxed text-faint"
          title={buildDate ? `built ${buildDate}` : ""}
        >
          <span class="text-dim">{build.version}</span>
          {#if build.commit && build.commit !== "-"}
            <span> · {build.commit}</span>
          {/if}
          {#if buildDate}
            <div>{buildDate}</div>
          {/if}
        </div>
      {/if}
    </div>
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

    <main class="min-w-0 flex-1 px-2 {view === 'repo' ? 'py-2' : 'max-w-[1280px] py-2'}">
      {#if view === "repos"}
        <Repos />
      {:else if view === "repo"}
        {#key repoId}
          <RepoDetail {repoId} />
        {/key}
      {:else if view === "sources"}
        <Sources sourceName={route.sourceName || ""} />
      {:else if view === "namespaces"}
        <Namespaces />
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
