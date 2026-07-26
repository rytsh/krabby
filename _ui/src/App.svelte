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
  import {
    buildOwnerTree,
    collapseTree,
    ancestorKeys,
    nodeKeys,
    sidebarPathMode,
  } from "./lib/paths.js";
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
    { href: "/search", label: "Search", icon: "search", match: (v) => v === "search" },
    { href: "/settings", label: "Settings", icon: "settings", match: (v) => v === "settings" },
    { href: "/about", label: "About", icon: "book", match: (v) => v === "about" },
  ];

  const title = {
    repos: "Repositories",
    repo: "Repository",
    sources: "Sources",
    namespaces: "Namespaces",
    activity: "Activity",
    search: "Search",
    settings: "Settings",
    about: "About",
  };

  // Expanded state per owner group, persisted so it survives reloads. Groups
  // default to collapsed: a group's repos are fetched lazily only when it is
  // expanded, so the sidebar stays cheap with many owners.
  //
  // Only what the user explicitly toggled lives here. Folders opened just to
  // reveal the repo being viewed go into `revealed` below and are deliberately
  // not persisted, so navigating to a repo does not silently turn into a
  // permanent "everything is open" preference after the next refresh.
  //
  // The key is versioned: v1 also stored the reveal chain, including keys for
  // rows that only exist in one path mode, and never pruned anything. That data
  // is the reason folders used to appear expanded on their own, so it is dropped
  // instead of migrated.
  const EXPANDED_KEY = "krabby-sidebar-expanded-v2";
  let expanded = $state({});
  try {
    localStorage.removeItem("krabby-sidebar-expanded");
    const saved = JSON.parse(localStorage.getItem(EXPANDED_KEY) || "{}") || {};
    expanded = Object.fromEntries(Object.entries(saved).filter(([, v]) => v === true));
  } catch {
    expanded = {};
  }

  function persistExpanded(map) {
    try {
      localStorage.setItem(EXPANDED_KEY, JSON.stringify(map));
    } catch {
      // Private mode / storage full: the state just won't survive a reload.
    }
  }

  // Nested folder tree built from the flat owner list, so that groups like
  // ".../parser" and ".../parser/poc" nest as parent/child. `fullTree` keeps the
  // long shared prefix chain (host/org/team/...) as separate levels; "smart"
  // mode collapses single-child prefix chains into one row. A collapsed row
  // always adopts its deepest child's key, so full-tree keys are a superset of
  // the smart-mode ones and can be used to validate persisted state in either
  // mode.
  let fullTree = $derived(buildOwnerTree($owners));
  let ownerTree = $derived($sidebarPathMode === "full" ? fullTree : collapseTree(fullTree));

  // Folders opened only to reveal the repo currently being viewed. Ephemeral:
  // recomputed on navigation, never written to localStorage.
  let revealed = $state({});

  // What the sidebar actually renders as open. A reveal wins over stored state
  // so the active repo is always visible; toggleNode drops the reveal when the
  // user closes such a folder, so it stays closable.
  let openKeys = $derived({ ...expanded, ...revealed });

  // A tree node is expandable by its full path key; when the node is also a real
  // owner group (node.owner != null) its repos are loaded lazily on expand.
  function toggleNode(node) {
    const next = !openKeys[node.key];

    const map = { ...expanded };
    if (next) map[node.key] = true;
    else delete map[node.key];
    expanded = map;
    persistExpanded(map);

    if (!next && revealed[node.key]) {
      const rest = { ...revealed };
      delete rest[node.key];
      revealed = rest;
    }

    if (next && node.owner !== null) loadOwnerRepos(node.owner);
  }

  // pruneExpanded drops persisted keys that have no row in the current tree
  // (removed repos, or keys written by older builds that invented prefixes).
  // Skipped while the owner list is still empty, otherwise the first render
  // would wipe everything.
  function pruneExpanded(tree) {
    const keys = nodeKeys(tree);
    if (keys.size === 0) return;

    const map = {};
    let dropped = false;
    for (const k of Object.keys(expanded)) {
      if (keys.has(k)) map[k] = true;
      else dropped = true;
    }
    if (!dropped) return;

    expanded = map;
    persistExpanded(map);
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

  // Walk the owner tree and fetch repos for every group that is already
  // expanded. loadOwnerRepos is cached, so this is a no-op for groups whose
  // repos are already loaded.
  function loadExpandedOwners(nodes) {
    for (const node of nodes) {
      if (!openKeys[node.key]) continue;
      if (node.owner !== null) loadOwnerRepos(node.owner);
      if (node.children.length > 0) loadExpandedOwners(node.children);
    }
  }

  // On load (and whenever the tree rebuilds) drop stale expand state and
  // restore the repos of the groups that were left expanded from a previous
  // session. Without the restore, expanded folders render empty after a page
  // refresh until the user toggles them, because repos are only fetched on
  // manual expand.
  $effect(() => {
    const full = fullTree;
    const tree = ownerTree;
    untrack(() => {
      pruneExpanded(full);
      loadExpandedOwners(tree);
    });
  });

  // When viewing a repo, reveal its owner group (and every folder above it) so
  // the active repo is visible and highlighted, and load that group's repos.
  // The reveal is derived from the route, so it disappears again as soon as the
  // user navigates elsewhere instead of sticking around in localStorage.
  $effect(() => {
    const tree = ownerTree;
    const id = view === "repo" ? repoId : "";

    if (!id) {
      if (untrack(() => Object.keys(revealed).length) > 0) revealed = {};
      return;
    }

    const owner = ownerOf(id);
    loadOwnerRepos(owner);

    const next = {};
    for (const key of ancestorKeys(tree, owner)) next[key] = true;

    // Reassigning an equivalent object on every tree rebuild would churn the
    // whole sidebar, so only write when the revealed chain actually changed.
    const same = untrack(() => {
      const keys = Object.keys(revealed);
      return keys.length === Object.keys(next).length && keys.every((k) => next[k]);
    });
    if (!same) {
      revealed = next;
      // Newly revealed folders can be owner groups themselves; fetch their
      // repos too (cached, so this is a no-op for the ones already loaded).
      untrack(() => loadExpandedOwners(tree));
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
        <RepoTree nodes={ownerTree} depth={0} expanded={openKeys} onToggle={toggleNode} {view} {repoId} />
      </nav>
    {/if}

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
              <span class="truncate text-[11px] text-faint">source code, repository docs, and connected sources</span>
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
