<script>
  // Web content sources: named collections (wikis, Confluence spaces) whose
  // pages are synced to markdown and indexed into the docs RAG. Each
  // collection is searchable on the Search page as "web:<name>".
  import { onMount } from "svelte";
  import { api } from "../lib/api.js";
  import { path as routePath, navigate, link } from "../lib/router.js";
  import { fmtDate, fmtEta } from "../lib/format.js";
  import Icon from "../lib/Icon.svelte";
  import Status from "../lib/Status.svelte";
  import MarkdownView from "../lib/MarkdownView.svelte";
  import { successToast } from "../lib/toast.js";

  // selected/doc come from the route: /sources/<name>?doc=<path>.
  let { sourceName = "" } = $props();
  let docParam = $derived.by(() => {
    const params = new URLSearchParams($routePath.split("?")[1] || "");
    return params.get("doc") || "";
  });
  let addContentParam = $derived.by(() => {
    const params = new URLSearchParams($routePath.split("?")[1] || "");
    return params.get("add") === "1";
  });

  let sources = $state([]);
  let selectedSource = $derived(sources.find((source) => source.name === sourceName));
  let loaded = $state(false);
  let error = $state("");

  // Per-collection page lists, loaded lazily when expanded.
  let pages = $state({});
  let expanded = $state({});
  // Per-collection distinct team names and the active team filter (JIRA).
  let teams = $state({});
  let teamFilter = $state({});
  let titleFilter = $state({});
  const titleSearchTimers = {};
  const pageLoadSeq = {};
  // Per-collection pagination: current page (1-based), total matching items and
  // whether more pages exist. The server pages the item list so large sources
  // (thousands of pages) are never loaded whole.
  const PER_PAGE = 50;
  let pageNum = $state({});
  let pageTotal = $state({});
  let pageHasMore = $state({});
  let pageLoading = $state({});

  // Doc viewer state.
  let docContent = $state("");
  let docURL = $state("");
  let docError = $state("");
  let deletingDoc = $state(false);

  // Add form.
  let showAdd = $state(false);
  let editingName = $state("");
  let adding = $state(false);
  let form = $state(newForm());
  let testingConfig = $state(false);
  let configTest = $state(null);
  let testedConfigSignature = $state("");

  function newForm() {
    return {
      name: "",
      type: "pages",
      description: "",
      refresh_interval: "24h",
      // Cron schedule(s), comma-separated (hardloop syntax, e.g. "0 2 * * *").
      // When set it is authoritative over refresh_interval, like repo schedules.
      schedule: "",
      base_url: "",
      space: "",
      user: "",
      api_token: "",
      include_labels: "",
      exclude_labels: "",
      // Confluence-only
      root_page: "",
      include_root: true,
      max_pages: "",
      // JIRA-only
      project: "",
      jql: "",
      include_subtasks: false,
      team_fields: "",
      max_issues: "",
      // Confluence + JIRA
      full_resync_schedule: "0 2 * * *",
    };
  }

  async function load() {
    try {
      sources = (await api.sources()) || [];
      error = "";
    } catch (e) {
      error = e.message;
    } finally {
      loaded = true;
    }
  }

  // Poll the source list while any source is actively syncing/indexing, so the
  // progress bar and page counts update live without a manual refresh.
  let pollTimer = null;
  function anyRunning() {
    return sources.some((s) => s.task_state || s.running || s.status === "fetching" || s.progress?.length);
  }
  $effect(() => {
    if (anyRunning() && !pollTimer) {
      pollTimer = setInterval(load, 2000);
    } else if (!anyRunning() && pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  });
  onMount(() => () => {
    if (pollTimer) clearInterval(pollTimer);
    for (const timer of Object.values(titleSearchTimers)) clearTimeout(timer);
  });

  // A sync runs one phase at a time, but the API reports a list (a repository
  // build runs several at once), so take the first.
  function phase(s) {
    return s?.progress?.[0] || null;
  }

  // Human progress label + percentage for a source's current phase.
  function progressPct(p) {
    if (!p || !p.total) return null;
    return Math.min(100, Math.round((p.done / p.total) * 100));
  }
  function progressLabel(p) {
    if (!p) return "";
    const phase = { fetch: "Fetching", index: "Embedding" }[p.phase] || p.phase;
    // Some providers page a cursor without publishing a result-set size: the
    // running count is still worth showing, just without a percentage.
    if (!p.total) return p.done ? `${phase} ${p.done}…` : `${phase}…`;
    return `${phase} ${p.done}/${p.total}`;
  }
  // The server only sends an estimate once the phase has run long enough to
  // produce a stable one, so an empty label here means "not known yet".
  function progressEta(p) {
    const eta = fmtEta(p?.eta_seconds);
    return eta ? `${eta} left` : "";
  }

  async function loadPages(name, page = pageNum[name] || 1) {
    const seq = (pageLoadSeq[name] || 0) + 1;
    pageLoadSeq[name] = seq;
    pageLoading = { ...pageLoading, [name]: true };
    try {
      const res = await api.source(name, teamFilter[name] || "", page, PER_PAGE, titleFilter[name] || "");
      if (pageLoadSeq[name] !== seq) return;
      pages = { ...pages, [name]: res?.pages || [] };
      pageNum = { ...pageNum, [name]: res?.page || page };
      pageTotal = { ...pageTotal, [name]: res?.total ?? (res?.pages?.length || 0) };
      pageHasMore = { ...pageHasMore, [name]: !!res?.has_more };
      // teams is the full distinct set across the collection (server-provided).
      if (res?.teams) teams = { ...teams, [name]: res.teams };
    } catch (e) {
      if (pageLoadSeq[name] !== seq) return;
      error = e.message;
    } finally {
      if (pageLoadSeq[name] === seq) pageLoading = { ...pageLoading, [name]: false };
    }
  }

  function setTitleFilter(name, value) {
    titleFilter = { ...titleFilter, [name]: value };
    clearTimeout(titleSearchTimers[name]);
    titleSearchTimers[name] = setTimeout(() => {
      pageNum = { ...pageNum, [name]: 1 };
      loadPages(name, 1);
    }, 300);
  }

  function clearTitleFilter(name) {
    clearTimeout(titleSearchTimers[name]);
    titleFilter = { ...titleFilter, [name]: "" };
    pageNum = { ...pageNum, [name]: 1 };
    loadPages(name, 1);
  }

  function goToPage(name, page) {
    if (page < 1) return;
    pageNum = { ...pageNum, [name]: page };
    loadPages(name, page);
  }

  function setTeamFilter(name, value) {
    teamFilter = { ...teamFilter, [name]: value };
    pageNum = { ...pageNum, [name]: 1 }; // reset to first page on filter change
    loadPages(name, 1);
  }

  function toggle(name) {
    expanded = { ...expanded, [name]: !expanded[name] };
    if (expanded[name] && !pages[name]) loadPages(name, pageNum[name] || 1);
  }

  // Human 1-based range of the current page, e.g. "1–50 of 4634".
  function pageRange(name) {
    const total = pageTotal[name] || 0;
    if (total === 0) return "0";
    const p = pageNum[name] || 1;
    const from = (p - 1) * PER_PAGE + 1;
    const to = Math.min(p * PER_PAGE, total);
    return `${from}\u2013${to} of ${total}`;
  }

  function lastPage(name) {
    return Math.max(1, Math.ceil((pageTotal[name] || 0) / PER_PAGE));
  }

  function sourceRefreshPolicy(source) {
    if (source?.specs?.length) return `Cron (server time): ${source.specs.join(", ")}`;
    if (source?.refresh_interval) return `Every ${source.refresh_interval}`;
    return "Manual only, via Sync now";
  }

  function splitLabels(s) {
    return s
      .split(",")
      .map((x) => x.trim())
      .filter(Boolean);
  }

  function providerConfig() {
    if (form.type === "confluence") {
      return {
        base_url: form.base_url.trim(),
        space: form.space.trim(),
        root_page: form.root_page.trim(),
        include_root: form.include_root,
        user: form.user.trim(),
        api_token: form.api_token,
        include_labels: splitLabels(form.include_labels),
        exclude_labels: splitLabels(form.exclude_labels),
        full_resync_schedule: form.full_resync_schedule.trim(),
        max_pages: form.max_pages ? Number(form.max_pages) : 0,
      };
    }
    if (form.type === "jira") {
      return {
        base_url: form.base_url.trim(),
        user: form.user.trim(),
        api_token: form.api_token,
        project: form.project.trim(),
        jql: form.jql.trim(),
        include_labels: splitLabels(form.include_labels),
        exclude_labels: splitLabels(form.exclude_labels),
        include_subtasks: form.include_subtasks,
        team_fields: splitLabels(form.team_fields),
        max_issues: form.max_issues ? Number(form.max_issues) : 0,
        full_resync_schedule: form.full_resync_schedule.trim(),
      };
    }
    return {};
  }

  function sourceBody() {
    const specs = form.schedule
      .split(",")
      .map((x) => x.trim())
      .filter(Boolean);
    return {
      name: form.name.trim(),
      type: form.type,
      description: form.description.trim(),
      refresh_interval: specs.length || form.refresh_interval === "manual" ? "" : form.refresh_interval,
      specs,
      config: providerConfig(),
    };
  }

  let configSignature = $derived(JSON.stringify(providerConfig()));

  async function add() {
    adding = true;
    try {
      const body = sourceBody();
      const isNewPageSource = !editingName && body.type === "pages";
      if (editingName) await api.updateSource(editingName, body);
      else await api.addSource(body);
      form = newForm();
      editingName = "";
      showAdd = false;
      configTest = null;
      testedConfigSignature = "";
      error = "";
      await load();
      if (isNewPageSource) navigate(`/sources/${encodeURIComponent(body.name)}?add=1`);
    } catch (e) {
      error = e.message;
    } finally {
      adding = false;
    }
  }

  async function testConfig() {
    const signature = configSignature;
    testingConfig = true;
    configTest = null;
    error = "";
    try {
      configTest = await api.testSourceConfig({
        type: form.type,
        existing_name: editingName || "",
        config: providerConfig(),
      });
      testedConfigSignature = signature;
    } catch (e) {
      configTest = { ok: false, error: e.message };
      testedConfigSignature = signature;
    } finally {
      testingConfig = false;
    }
  }

  function editSource(source, e) {
    e.stopPropagation();
    editingName = source.name;
    form = {
      name: source.name,
      type: source.type,
      description: source.description || "",
      refresh_interval: source.refresh_interval || "manual",
      schedule: (source.specs || []).join(", "),
      base_url: source.config?.base_url || "",
      space: source.config?.space || "",
      user: source.config?.user || "",
      api_token: "",
      include_labels: (source.config?.include_labels || []).join(", "),
      exclude_labels: (source.config?.exclude_labels || []).join(", "),
      root_page: source.config?.root_page || "",
      include_root: source.config?.include_root !== false,
      max_pages: source.config?.max_pages || "",
      project: source.config?.project || "",
      jql: source.config?.jql || "",
      include_subtasks: source.config?.include_subtasks === true,
      team_fields: (source.config?.team_fields || []).join(", "),
      max_issues: source.config?.max_issues || "",
      full_resync_schedule: source.config?.full_resync_schedule || "0 2 * * *",
    };
    configTest = null;
    testedConfigSignature = "";
    showAdd = true;
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  function closeForm() {
    showAdd = false;
    editingName = "";
    form = newForm();
    configTest = null;
    testedConfigSignature = "";
  }

  async function refresh(name, e) {
    e.stopPropagation();
    try {
      await api.refreshSource(name);
      successToast("Sync queued");
      await load();
    } catch (err) {
      error = err.message;
    }
  }

  let canceling = $state({});
  async function cancel(name, e) {
    e.stopPropagation();
    canceling = { ...canceling, [name]: true };
    try {
      await api.cancelSource(name);
      successToast("Cancel requested");
      await load();
    } catch (err) {
      error = err.message;
    } finally {
      canceling = { ...canceling, [name]: false };
    }
  }

  async function remove(name, e) {
    e.stopPropagation();
    if (!confirm(`Delete source "${name}", its synced pages and index entries?`)) return;
    try {
      await api.deleteSource(name);
      await load();
    } catch (err) {
      error = err.message;
    }
  }

  // Per-collection "add page" inputs (pages type).
  let manualTitle = $state("");
  let manualMarkdown = $state("");
  let savingManualPage = $state(false);
  let pageUrl = $state({});
  let sitemapUrl = $state({});
  let importingSitemap = $state({});
  let browserImporting = $state({});
  let browserProgress = $state({});
  let corsRelay = $state("");
  let extensionAvailable = $state(false);
  let extensionVersion = $state("");
  let appVersion = $state("");
  let extensionOutdated = $derived(
    extensionAvailable && !!appVersion && !!extensionVersion && extensionVersion !== appVersion,
  );
  const extensionRequests = new Map();

  function handleExtensionMessage(event) {
    if (event.source !== window || event.origin !== location.origin) return;
    if (event.data?.type === "KRABBY_EXTENSION_READY") {
      extensionAvailable = true;
      extensionVersion = event.data.version || "unknown";
      return;
    }
    if (event.data?.type !== "KRABBY_EXTENSION_RESPONSE") return;
    const pending = extensionRequests.get(event.data.requestId);
    if (!pending) return;
    extensionRequests.delete(event.data.requestId);
    clearTimeout(pending.timer);
    if (event.data.error) pending.reject(new Error(event.data.error));
    else pending.resolve(event.data.result);
  }

  function extensionRequest(action, url) {
    if (!extensionAvailable) return Promise.reject(new Error("Krabby browser extension is not connected"));
    const requestId = crypto.randomUUID();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        extensionRequests.delete(requestId);
        reject(new Error(`Browser extension timed out while processing ${url}`));
      }, 60000);
      extensionRequests.set(requestId, { resolve, reject, timer });
      window.postMessage({ type: "KRABBY_EXTENSION_REQUEST", requestId, action, url }, location.origin);
    });
  }

  function saveCorsRelay(value) {
    corsRelay = value;
    localStorage.setItem("krabby-cors-relay", value);
  }

  function relayURL(target) {
    const relay = corsRelay.trim();
    if (!relay) return "";
    if (relay.includes("{rawUrl}")) return relay.replaceAll("{rawUrl}", target);
    if (relay.includes("{url}")) return relay.replaceAll("{url}", encodeURIComponent(target));
    return relay + encodeURIComponent(target);
  }

  // Browser imports first try the original URL. Sites that do not opt into
  // CORS can be reached through a relay the user controls; no page content is
  // silently sent through a hard-coded third-party service.
  async function browserFetchText(url) {
    let directError;
    try {
      const response = await fetch(url, { credentials: "omit" });
      if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
      return await response.text();
    } catch (e) {
      directError = e;
    }

    if (extensionAvailable) {
      try {
        return await extensionRequest("fetch", url);
      } catch (e) {
        directError = e;
      }
    }

    const proxied = relayURL(url);
    if (!proxied) {
      throw new Error(
        `Browser could not read ${url} (${directError?.message || "CORS blocked"}). Configure a CORS relay below and retry.`,
      );
    }
    const response = await fetch(proxied);
    if (!response.ok) throw new Error(`CORS relay returned ${response.status} ${response.statusText} for ${url}`);
    return await response.text();
  }

  async function browserFetchPage(url) {
    if (extensionAvailable) return await extensionRequest("render", url);
    return await browserFetchText(url);
  }

  function prepareBrowserHTML(html, url) {
    const doc = new DOMParser().parseFromString(html, "text/html");
    const appShell = !!doc.querySelector("#app:empty, #root:empty, #__next:empty, [data-reactroot]:empty");
    doc.querySelectorAll("script, style, noscript, template, svg, canvas, iframe").forEach((node) => node.remove());

    const contentRoot = doc.querySelector("main, article, [role='main']") || doc.body;
    const visibleText = (contentRoot?.textContent || "").replace(/\s+/g, " ").trim();
    if (visibleText.length < 200 || (appShell && visibleText.length < 1000)) {
      throw new Error(
        `${url} appears to be an unrendered JavaScript page (${visibleText.length} visible characters). Use a rendering relay or browser extension.`,
      );
    }

    const compact = document.implementation.createHTMLDocument(doc.title || "");
    compact.body.append(contentRoot.cloneNode(true));
    return compact.documentElement.outerHTML;
  }

  function parseSitemap(xml, url) {
    const doc = new DOMParser().parseFromString(xml, "application/xml");
    const parseError = doc.querySelector("parsererror");
    if (parseError) throw new Error(`Invalid sitemap XML at ${url}: ${parseError.textContent.trim()}`);
    const root = doc.documentElement?.localName;
    if (root !== "urlset" && root !== "sitemapindex") {
      throw new Error(`Unsupported sitemap root "${root || "unknown"}" at ${url}`);
    }
    const locations = [...doc.getElementsByTagNameNS("*", "loc")]
      .map((node) => node.textContent.trim())
      .filter(Boolean)
      .map((location) => new URL(location, url).href);
    return { root, locations };
  }

  async function browserSitemapURLs(rootURL) {
    const pending = [rootURL];
    const seenSitemaps = new Set();
    const seenPages = new Set();
    while (pending.length) {
      const current = pending.shift();
      if (seenSitemaps.has(current)) continue;
      if (seenSitemaps.size >= 100) throw new Error("Sitemap index exceeds 100 files");
      seenSitemaps.add(current);
      const parsed = parseSitemap(await browserFetchText(current), current);
      if (parsed.root === "sitemapindex") {
        pending.push(...parsed.locations.filter((url) => !seenSitemaps.has(url)));
      } else {
        for (const url of parsed.locations) {
          if (seenPages.size >= 50000) throw new Error("Sitemap exceeds 50,000 page URLs");
          seenPages.add(url);
        }
      }
    }
    return [...seenPages];
  }

  async function addPage(name) {
    const url = (pageUrl[name] || "").trim();
    if (!url) return;
    try {
      await api.addSourcePage(name, url);
      pageUrl = { ...pageUrl, [name]: "" };
      await loadPages(name);
      await load();
    } catch (e) {
      error = e.message;
    }
  }

  async function addMarkdownPage(name) {
    const title = manualTitle.trim();
    const content = manualMarkdown.trim();
    if (!title || !content) return;
    savingManualPage = true;
    error = "";
    try {
      await api.importSourcePages(name, [{ title, content_type: "text/markdown", content }]);
      manualTitle = "";
      manualMarkdown = "";
      successToast("Markdown page saved and indexed");
      await loadPages(name, 1);
      await load();
    } catch (e) {
      error = e.message;
    } finally {
      savingManualPage = false;
    }
  }

  async function importPageInBrowser(name) {
    const url = (pageUrl[name] || "").trim();
    if (!url) return;
    browserImporting = { ...browserImporting, [name]: true };
    browserProgress = { ...browserProgress, [name]: "Fetching page in browser..." };
    try {
      const content = prepareBrowserHTML(await browserFetchPage(url), url);
      browserProgress = { ...browserProgress, [name]: "Converting and indexing..." };
      await api.importSourcePages(name, [{ url, content_type: "text/html", content }]);
      pageUrl = { ...pageUrl, [name]: "" };
      browserProgress = { ...browserProgress, [name]: "" };
      successToast("Browser-fetched page imported");
      await loadPages(name, 1);
      await load();
    } catch (e) {
      error = e.message;
      browserProgress = { ...browserProgress, [name]: "" };
    } finally {
      browserImporting = { ...browserImporting, [name]: false };
    }
  }

  async function importSitemap(name) {
    const url = (sitemapUrl[name] || "").trim();
    if (!url) return;
    importingSitemap = { ...importingSitemap, [name]: true };
    try {
      const result = await api.importSourceSitemap(name, url);
      sitemapUrl = { ...sitemapUrl, [name]: "" };
      successToast(`Imported ${result?.added || 0} pages (${result?.existing || 0} already present)`);
      await loadPages(name, 1);
      await load();
    } catch (e) {
      error = e.message;
    } finally {
      importingSitemap = { ...importingSitemap, [name]: false };
    }
  }

  async function importSitemapInBrowser(name) {
    const url = (sitemapUrl[name] || "").trim();
    if (!url) return;
    error = "";
    browserImporting = { ...browserImporting, [name]: true };
    browserProgress = { ...browserProgress, [name]: "Reading sitemap in browser..." };
    try {
      const urls = await browserSitemapURLs(url);
      if (!urls.length) throw new Error("Sitemap contains no page URLs");
      if (urls.length > 200 && !confirm(`Fetch and import ${urls.length} pages through this browser?`)) {
        browserProgress = { ...browserProgress, [name]: "" };
        return;
      }

      let imported = 0;
      let emptyBatches = 0;
      const failures = [];
      const batchSize = 5;
      for (let offset = 0; offset < urls.length; offset += batchSize) {
        const batchURLs = urls.slice(offset, offset + batchSize);
        browserProgress = {
          ...browserProgress,
          [name]: `Fetching pages ${offset + 1}-${Math.min(offset + batchURLs.length, urls.length)} of ${urls.length}...`,
        };
        const fetched = await Promise.allSettled(
          batchURLs.map(async (pageURL) => ({
            url: pageURL,
            content_type: "text/html",
            content: prepareBrowserHTML(await browserFetchPage(pageURL), pageURL),
          })),
        );
        const pagesToImport = [];
        fetched.forEach((item, index) => {
          if (item.status === "fulfilled") pagesToImport.push(item.value);
          else failures.push(`${batchURLs[index]}: ${item.reason?.message || "fetch failed"}`);
        });

        let batchImported = 0;
        if (pagesToImport.length) {
          browserProgress = {
            ...browserProgress,
            [name]: `Indexing ${imported + 1}-${imported + pagesToImport.length} of ${urls.length}...`,
          };
          try {
            const result = await api.importSourcePages(name, pagesToImport);
            batchImported = result?.imported || pagesToImport.length;
          } catch {
            // A malformed page makes the backend reject its whole atomic batch.
            // Retry individually so one bad URL does not discard its neighbors.
            for (const page of pagesToImport) {
              try {
                const result = await api.importSourcePages(name, [page]);
                batchImported += result?.imported || 1;
              } catch (pageError) {
                failures.push(`${page.url}: ${pageError.message}`);
              }
            }
          }
        }

        imported += batchImported;
        if (batchImported > 0) {
          emptyBatches = 0;
        } else {
          emptyBatches++;
          const detail = failures.slice(-3).join("; ");
          if (imported === 0 || emptyBatches >= 3) {
            throw new Error(`No pages could be imported; stopped early. ${detail}`);
          }
        }
      }

      sitemapUrl = { ...sitemapUrl, [name]: "" };
      browserProgress = { ...browserProgress, [name]: "" };
      await loadPages(name, 1);
      await load();
      const summary = `Browser imported ${imported} pages${failures.length ? `; ${failures.length} failed` : ""}`;
      successToast(summary);
      error = failures.length ? `${summary}. ${failures.slice(0, 3).join("; ")}` : "";
    } catch (e) {
      browserProgress = { ...browserProgress, [name]: "" };
      await loadPages(name, 1);
      await load();
      error = e.message;
    } finally {
      browserImporting = { ...browserImporting, [name]: false };
    }
  }

  async function removePage(name, slug) {
    if (!confirm(`Remove page ${slug}?`)) return;
    try {
      await api.deleteSourcePage(name, slug);
      await loadPages(name);
      await load();
    } catch (e) {
      error = e.message;
    }
  }

  async function removeCurrentDoc() {
    const slug = docParam.endsWith(".md") ? docParam.slice(0, -3) : docParam;
    if (!slug || !confirm(`Delete ${slug} from ${sourceName}?`)) return;
    deletingDoc = true;
    try {
      await api.deleteSourcePage(sourceName, slug);
      successToast("Page and its index entries deleted");
      navigate("/sources");
    } catch (e) {
      docError = e.message;
    } finally {
      deletingDoc = false;
    }
  }

  // Load the markdown of the routed doc (deep link from search results).
  $effect(() => {
    const name = sourceName;
    const doc = docParam;
    if (!name || !doc) {
      docContent = "";
      docURL = "";
      docError = "";
      return;
    }
    docContent = "";
    docURL = "";
    docError = "";
    api
      .sourceDoc(name, doc)
      .then((res) => {
        docContent = res?.content || "";
        docURL = res?.url || "";
      })
      .catch((e) => (docError = e.message));
  });

  onMount(() => {
    corsRelay = localStorage.getItem("krabby-cors-relay") || "";
    load();
    api.settings().then((settings) => (appVersion = settings?.version || "")).catch(() => {});
  });
  onMount(() => {
    window.addEventListener("message", handleExtensionMessage);
    window.postMessage({ type: "KRABBY_EXTENSION_PING" }, location.origin);
    return () => {
      window.removeEventListener("message", handleExtensionMessage);
      for (const pending of extensionRequests.values()) {
        clearTimeout(pending.timer);
        pending.reject(new Error("Page closed"));
      }
      extensionRequests.clear();
    };
  });
</script>

{#if sourceName && addContentParam}
  <div class="mb-3 flex items-center gap-2 text-[13px]">
    <a href="/sources" use:link class="text-dim transition-colors hover:text-fg">Sources</a>
    <span class="text-faint">/</span>
    <span class="font-mono">{sourceName}</span>
    <span class="text-faint">/</span>
    <span>Add content</span>
  </div>

  {#if error}
    <div class="mb-3 rounded-md border border-err bg-err/10 px-3 py-2.5 text-[13px] text-err">{error}</div>
  {/if}

  {#if !loaded}
    <div class="card p-6 text-center text-dim">Loading…</div>
  {:else if !selectedSource}
    <div class="card p-6 text-center text-err">Source not found.</div>
  {:else if selectedSource.type !== "pages"}
    <div class="card p-6 text-center text-dim">
      Content is discovered by the {selectedSource.type} provider and cannot be added manually.
    </div>
  {:else}
    <div class="card overflow-hidden">
      <div class="border-b border-line bg-surface-2 px-4 py-3">
        <div class="flex items-center gap-2">
          <Icon name="plus" size={16} />
          <h2 class="m-0 text-[15px] font-semibold">Add content to <span class="font-mono">{sourceName}</span></h2>
        </div>
        <p class="mb-0 mt-1 text-[12px] text-faint">
          Write Markdown directly, fetch one page, or crawl a sitemap.
        </p>
      </div>
      <div class="grid gap-4 p-4">
        <section class="flex items-start gap-2.5 rounded-md border border-line bg-surface-2 px-3 py-2.5">
          <span class="mt-0.5 shrink-0 text-dim"><Icon name="refresh" size={15} /></span>
          <div class="min-w-0 text-[12px] text-dim">
            <div class="font-medium text-fg">Remote page refresh: {sourceRefreshPolicy(selectedSource)}</div>
            <div class="mt-0.5 text-faint">
              URL and sitemap pages follow this source-level policy. Manually written Markdown is stored as-is and
              is never fetched or overwritten by scheduled refreshes.
              {#if selectedSource.last_refresh_at}
                Last source sync: {fmtDate(selectedSource.last_refresh_at)}.
              {/if}
            </div>
          </div>
        </section>

        <section class="grid gap-2">
          <div>
            <div class="text-[13px] font-medium">Write Markdown</div>
            <p class="mb-0 mt-0.5 text-[12px] text-faint">
              Stored in Krabby without a remote URL, so scheduled refreshes leave it unchanged. The title identifies
              the page; saving the same title again updates it.
            </p>
          </div>
          <label class="flex flex-col gap-1 text-[12px] text-faint">
            Title
            <input
              class="input text-[13px]"
              placeholder="e.g. Production recovery runbook"
              bind:value={manualTitle}
            />
          </label>
          <label class="flex flex-col gap-1 text-[12px] text-faint">
            Markdown
            <textarea
              class="input min-h-64 resize-y font-mono text-[13px] leading-5"
              placeholder={"## Overview\n\nWrite or paste Markdown here..."}
              bind:value={manualMarkdown}
              spellcheck="true"
            ></textarea>
          </label>
          <div class="flex justify-end">
            <button
              class="btn btn-primary"
              onclick={() => addMarkdownPage(sourceName)}
              disabled={savingManualPage || !manualTitle.trim() || !manualMarkdown.trim()}
            >
              {savingManualPage ? "Saving…" : "Save Markdown"}
            </button>
          </div>
        </section>

        <section class="grid gap-2 border-t border-line pt-4">
          <div class="text-[13px] font-medium">Single page</div>
          <div class="flex flex-wrap gap-2">
            <input
              class="input min-w-64 flex-1"
              placeholder="https://wiki.example.com/page"
              value={pageUrl[sourceName] || ""}
              oninput={(e) => (pageUrl = { ...pageUrl, [sourceName]: e.target.value })}
              onkeydown={(e) => e.key === "Enter" && addPage(sourceName)}
            />
            <button class="btn" onclick={() => addPage(sourceName)} disabled={!(pageUrl[sourceName] || "").trim()}>
              Fetch on server
            </button>
            <button
              class="btn btn-primary"
              onclick={() => importPageInBrowser(sourceName)}
              disabled={browserImporting[sourceName] || !(pageUrl[sourceName] || "").trim()}
            >
              {extensionAvailable ? "Render with extension" : "Fetch in browser"}
            </button>
          </div>
        </section>

        <section class="grid gap-2 border-t border-line pt-4">
          <div class="text-[13px] font-medium">Sitemap</div>
          <div class="flex flex-wrap gap-2">
            <input
              class="input min-w-64 flex-1"
              placeholder="https://example.com/sitemap.xml"
              value={sitemapUrl[sourceName] || ""}
              oninput={(e) => (sitemapUrl = { ...sitemapUrl, [sourceName]: e.target.value })}
              onkeydown={(e) => e.key === "Enter" && importSitemap(sourceName)}
            />
            <button
              class="btn"
              onclick={() => importSitemap(sourceName)}
              disabled={importingSitemap[sourceName] || !(sitemapUrl[sourceName] || "").trim()}
            >
              {importingSitemap[sourceName] ? "Importing…" : "Fetch on server"}
            </button>
            <button
              class="btn btn-primary"
              onclick={() => importSitemapInBrowser(sourceName)}
              disabled={browserImporting[sourceName] || !(sitemapUrl[sourceName] || "").trim()}
            >
              {browserImporting[sourceName]
                ? "Importing…"
                : extensionAvailable
                  ? "Render with extension"
                  : "Fetch in browser"}
            </button>
          </div>
        </section>

        <div
          class={`flex flex-col gap-2 rounded border px-3 py-2 text-[12px] sm:flex-row sm:items-center ${extensionOutdated ? "border-warn bg-warn/10 text-warn" : extensionAvailable ? "border-ok bg-ok/10 text-ok" : "border-line bg-surface-2 text-faint"}`}
        >
          <div class="min-w-0 flex-1">
            {#if extensionOutdated}
              Extension version {extensionVersion} does not match Krabby {appVersion}. Download and reload the current extension.
            {:else if extensionAvailable}
              Browser extension {extensionVersion} connected. Pages will be captured after rendering.
            {:else}
              Browser extension not detected. Download the ZIP, extract it, load the folder from
              <code class="font-mono">chrome://extensions</code>, then connect this origin from the extension popup.
            {/if}
          </div>
          <a class="btn btn-sm shrink-0" href="api/v1/browser-extension.zip">
            {extensionOutdated ? "Download update" : "Download extension"}
          </a>
        </div>

        <label class="flex flex-col gap-1 border-t border-line pt-4 text-[12px] text-faint">
          Optional CORS relay (kept only in this browser)
          <input
            class="input font-mono"
            placeholder={"http://127.0.0.1:8080/?url={url}"}
            value={corsRelay}
            oninput={(e) => saveCorsRelay(e.target.value)}
          />
          <span>
            Browser fetch is direct when the site permits CORS. For blocked sites such as docs.n8n.io,
            use a relay you trust; <code class="font-mono">{`{url}`}</code> receives the encoded target and
            <code class="font-mono">{`{rawUrl}`}</code> the original URL.
          </span>
        </label>

        {#if browserProgress[sourceName]}
          <div class="rounded border border-line bg-surface-2 px-3 py-2 text-[12px] text-busy">
            {browserProgress[sourceName]}
          </div>
        {/if}
      </div>
    </div>
  {/if}
{:else if sourceName && docParam}
  <!-- Doc viewer: /sources/<name>?doc=<path> -->
  <div class="mb-3 flex items-center gap-2 text-[13px]">
    <a href="/sources" use:link class="text-dim transition-colors hover:text-fg">Sources</a>
    <span class="text-faint">/</span>
    <span class="font-mono">{sourceName}</span>
    <span class="text-faint">/</span>
    <span class="truncate font-mono text-dim">{docParam}</span>
    {#if docURL || selectedSource?.type === "pages"}
      <span class="ml-auto flex shrink-0 items-center gap-2">
        {#if docURL}
          <a class="btn btn-sm" href={docURL} target="_blank" rel="noreferrer noopener">
            Open original
          </a>
        {/if}
        {#if selectedSource?.type === "pages"}
          <button class="btn btn-sm btn-danger" onclick={removeCurrentDoc} disabled={deletingDoc}>
            {deletingDoc ? "Deleting…" : "Delete"}
          </button>
        {/if}
      </span>
    {/if}
  </div>
  {#if docError}
    <div class="card p-6 text-center text-err">{docError}</div>
  {:else if !docContent}
    <div class="card p-6 text-center text-dim">Loading…</div>
  {:else}
    <div class="card p-5">
      <MarkdownView markdown={docContent} />
    </div>
  {/if}
{:else}
  <p class="text-dim">
    Non-git content sources: wikis and Confluence spaces synced to markdown and searchable via docs
    search as <code class="font-mono text-[12px]">web:&lt;name&gt;</code>.
  </p>

  {#if error}
    <div class="mt-3 rounded-md border border-err bg-err/10 px-3 py-2.5 text-[13px] text-err">{error}</div>
  {/if}

  <div class="my-4">
    {#if !showAdd}
      <button class="btn btn-primary" onclick={() => (showAdd = true)}>Add source</button>
    {:else}
      <div class="card flex flex-col gap-3 p-4">
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Name (search scope)
            <input class="input" placeholder="e.g. wine" bind:value={form.name} disabled={!!editingName} />
          </label>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Type
            <select class="input" bind:value={form.type} disabled={!!editingName}>
              <option value="pages">Custom web (URL list)</option>
              <option value="confluence">Confluence space</option>
              <option value="jira">JIRA project / JQL</option>
            </select>
          </label>
          <label class="flex flex-col gap-1 text-[13px] text-dim">
            Auto refresh {form.schedule.trim() ? "(overridden by schedule)" : ""}
            <select class="input" bind:value={form.refresh_interval} disabled={!!form.schedule.trim()}>
              <option value="manual">manual only</option>
              <option value="1h">every hour</option>
              <option value="6h">every 6 hours</option>
              <option value="24h">daily</option>
              <option value="168h">weekly</option>
            </select>
          </label>
        </div>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Cron schedule (optional; comma-separated, overrides auto refresh — same as repos)
          <input
            class="input font-mono"
            placeholder="0 2 * * *,  @every 6h"
            bind:value={form.schedule}
          />
          <span class="text-[11px] text-faint">
            e.g. <code class="font-mono">0 2 * * *</code> (daily 02:00),
            <code class="font-mono">@every 6h</code>, or several separated by commas. Leave empty to use Auto refresh.
          </span>
        </label>

        <label class="flex flex-col gap-1 text-[13px] text-dim">
          Description (what this source holds — shown to MCP/AI to pick the right source)
          <input
            class="input"
            placeholder="e.g. Delivery Support runbooks and TERs"
            bind:value={form.description}
          />
        </label>

        {#if form.type === "confluence"}
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Base URL
              <input class="input" placeholder="https://acme.atlassian.net/wiki" bind:value={form.base_url} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Space key {form.root_page ? "(optional when root page set)" : ""}
              <input class="input" placeholder="FinOps" bind:value={form.space} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Root page id (index this page + all descendants only)
              <input class="input" placeholder="1254228318" bind:value={form.root_page} />
            </label>
            {#if form.root_page}
              <label class="flex items-center gap-2 text-[13px] text-dim sm:col-span-2">
                <input type="checkbox" bind:checked={form.include_root} />
                Also index the root page itself
              </label>
            {/if}
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              User (email; empty = bearer token)
              <input class="input" placeholder="me@acme.com" bind:value={form.user} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              API token {editingName ? "(blank = keep existing)" : ""}
              <input class="input" type="password" bind:value={form.api_token} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Include labels (comma separated; empty = all pages)
              <input class="input" placeholder="public, docs" bind:value={form.include_labels} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Exclude labels (comma separated)
              <input class="input" placeholder="draft, archived" bind:value={form.exclude_labels} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Full re-sync schedule (cron; reconciles deletions)
              <input class="input font-mono" placeholder="0 2 * * *" bind:value={form.full_resync_schedule} />
              <span class="text-[11px] text-faint">Default: daily at 02:00 in the server timezone.</span>
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Max pages per sync (0 = no limit)
              <input class="input" type="number" min="0" placeholder="0" bind:value={form.max_pages} />
            </label>
          </div>
          <p class="m-0 text-[12px] text-faint">
            Set a <strong>Root page id</strong> (from the page URL) to index just that page and its
            whole sub-tree — register several sub-trees of one space as separate keyed sources (e.g.
            <code class="font-mono">delivery-support</code>). Leave it empty to index the whole space.
          </p>
        {:else if form.type === "jira"}
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Base URL
              <input class="input" placeholder="https://jira.acme.com" bind:value={form.base_url} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Project key (or use JQL below)
              <input class="input" placeholder="OFS" bind:value={form.project} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim sm:col-span-2">
              JQL (optional; overrides project)
              <input
                class="input"
                placeholder="project = OFS AND updated >= -30d ORDER BY updated DESC"
                bind:value={form.jql}
              />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              User (email; empty = bearer token / PAT)
              <input class="input" placeholder="me@acme.com" bind:value={form.user} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              API token / PAT {editingName ? "(blank = keep existing)" : ""}
              <input class="input" type="password" bind:value={form.api_token} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Include labels (comma separated; empty = all)
              <input class="input" placeholder="customer, prod" bind:value={form.include_labels} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Skip labels (comma separated)
              <input class="input" placeholder="wontfix, duplicate" bind:value={form.exclude_labels} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Team field ids (comma separated custom fields)
              <input class="input" placeholder="customfield_104705, customfield_110643" bind:value={form.team_fields} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Max issues per sync (0 = no limit)
              <input class="input" type="number" min="0" placeholder="0" bind:value={form.max_issues} />
            </label>
            <label class="flex flex-col gap-1 text-[13px] text-dim">
              Full re-sync schedule (cron; reconciles deletions)
              <input class="input font-mono" placeholder="0 2 * * *" bind:value={form.full_resync_schedule} />
              <span class="text-[11px] text-faint">Default: daily at 02:00 in the server timezone.</span>
            </label>
            <label class="flex items-center gap-2 text-[13px] text-dim sm:col-span-2">
              <input type="checkbox" bind:checked={form.include_subtasks} />
              Also index sub-tasks
            </label>
          </div>
          <p class="m-0 text-[12px] text-faint">
            Sub-tasks are skipped by default: they carry a fragment of their parent's story with
            none of the context that makes the parent answerable, so indexing them adds
            near-duplicate chunks that compete with the parent ticket. Turning this off on an
            existing source drops the already-synced sub-tasks on the next sync.
          </p>
          <p class="m-0 text-[12px] text-faint">
            Tickets are streamed as they are fetched, so a project of any size syncs in the same
            memory — leave <strong>Max issues</strong> at 0 unless you want to bound the time and API
            spend of a single run. A capped sync cannot tell a deleted ticket from one past the cap,
            so it stops reconciling deletions until an uncapped full pass runs.
          </p>
          <p class="m-0 text-[12px] text-faint">
            Team field ids are instance-specific JIRA custom fields that hold team/squad ownership
            (e.g. a "Squad" field). Their values are indexed so tickets are searchable by team name.
          </p>
        {:else}
          <p class="m-0 text-[12px] text-faint">
            Add page URLs or import a sitemap after creating the collection. Private pages resolve
            auth from the git credentials store by URL pattern.
          </p>
        {/if}

        {#if configTest}
          {#if testedConfigSignature !== configSignature}
            <div class="rounded-md border border-warn bg-warn/10 px-3 py-2.5 text-[13px] text-warn">
              Provider settings changed after the test. Run it again before saving.
            </div>
          {:else if configTest.ok}
            <div class="rounded-md border border-ok bg-ok/10 px-3 py-2.5 text-[13px] text-ok">
              <strong>{configTest.item_count} items</strong> would be indexed from {configTest.scanned} checked
              {#if configTest.total} ({configTest.total} returned by JIRA){/if}.
              Test completed in {configTest.latency_ms}ms.
              {#if configTest.truncated}
                <span class="block text-warn">
                  Count was truncated by the configured limit of {configTest.limit}; more items may match.
                </span>
              {/if}
            </div>
          {:else}
            <div class="rounded-md border border-err bg-err/10 px-3 py-2.5 text-[13px] text-err">
              Test failed: {configTest.error}
            </div>
          {/if}
        {/if}

        <div class="flex flex-wrap gap-2">
          {#if form.type === "jira" || form.type === "confluence"}
            <button class="btn" onclick={testConfig} disabled={testingConfig || adding}>
              {testingConfig ? "Testing…" : "Test & preview"}
            </button>
          {/if}
          <button class="btn btn-primary" onclick={add} disabled={adding || testingConfig || !form.name.trim()}>
            {adding ? "Saving…" : editingName ? "Save source" : "Create source"}
          </button>
          <button class="btn" onclick={closeForm}>Cancel</button>
        </div>
      </div>
    {/if}
  </div>

  <div class="flex flex-col gap-3">
    {#if !loaded}
      <div class="card p-6 text-center text-dim">Loading…</div>
    {:else if sources.length === 0}
      <div class="card p-6 text-center text-dim">No web sources yet.</div>
    {:else}
      {#each sources as s (s.name)}
        <div class="card overflow-hidden">
          <div class="flex items-center hover:bg-surface-2">
            <button
              class="flex min-w-0 flex-1 cursor-pointer items-center gap-2.5 px-3.5 py-2.5 text-left"
              onclick={() => toggle(s.name)}
              aria-expanded={!!expanded[s.name]}
            >
            <Icon name={expanded[s.name] ? "chevron-down" : "chevron-right"} size={14} />
            <Icon name={s.type === "confluence" ? "book" : s.type === "jira" ? "tag" : "search"} size={14} />
            <span class="font-mono text-[13.5px] font-medium">{s.name}</span>
            <span class="rounded border border-line px-1.5 text-[11px] text-dim">{s.type}</span>
            <span class="font-mono text-[11px] text-faint">web:{s.name}</span>
            <span class="ml-auto flex items-center gap-2.5 text-[12px] text-faint">
              {#if phase(s)}
                {@const p = phase(s)}
                <span class="flex items-center gap-1.5 text-busy">
                  {#if progressPct(p) !== null}
                    <span class="inline-block h-1.5 w-20 overflow-hidden rounded-full bg-surface-3">
                      <span class="block h-full rounded-full bg-busy transition-all" style="width: {progressPct(p)}%"></span>
                    </span>
                    <span class="font-mono">{progressLabel(p)} ({progressPct(p)}%)</span>
                    {#if progressEta(p)}
                      <span class="text-faint" title="Estimated from the rate so far">· {progressEta(p)}</span>
                    {/if}
                  {:else}
                    <span>{progressLabel(p)}</span>
                  {/if}
                </span>
              {:else if s.running}
                <span class="text-busy">({s.running})</span>
              {:else if s.task_state === "queued"}
                <span class="text-warn">(queued)</span>
              {/if}
              <span>{s.page_count} {s.page_count === 1 ? "page" : "pages"}</span>
              <Status status={s.status} />
            </span>
            </button>
            {#if s.type === "pages"}
              <button
                class="btn btn-sm mr-3 shrink-0"
                onclick={() => navigate(`/sources/${encodeURIComponent(s.name)}?add=1`)}
              >
                Add content
              </button>
            {/if}
          </div>

          {#if expanded[s.name]}
            <div class="border-t border-line px-3.5 py-3">
              {#if s.description}
                <p class="mb-2 text-[12.5px] text-dim">{s.description}</p>
              {/if}
              <div class="mb-3 flex flex-wrap items-center gap-x-5 gap-y-1 text-[12px] text-faint">
                <span>Last sync: {s.last_refresh_at ? fmtDate(s.last_refresh_at) : "never"}</span>
                {#if s.specs?.length}
                  <span>Remote refresh: <span class="font-mono">{sourceRefreshPolicy(s)}</span></span>
                {:else}
                  <span>Remote refresh: {sourceRefreshPolicy(s)}</span>
                {/if}
                {#if s.type === "confluence"}
                  <span class="font-mono">
                    {s.config?.base_url}
                    {s.config?.root_page ? `· page ${s.config.root_page} + subtree` : `· ${s.config?.space}`}
                  </span>
                  {#if s.config?.include_labels?.length}
                    <span>labels: {s.config.include_labels.join(", ")}</span>
                  {/if}
                {/if}
                {#if s.type === "jira"}
                  <span class="font-mono">{s.config?.base_url} · {s.config?.jql || s.config?.project}</span>
                  {#if s.config?.exclude_labels?.length}
                    <span>skip: {s.config.exclude_labels.join(", ")}</span>
                  {/if}
                  <span>{s.config?.include_subtasks ? "with sub-tasks" : "no sub-tasks"}</span>
                {/if}
                {#if s.last_error}
                  <span class="text-err" title={s.last_error}>error: {s.last_error.slice(0, 120)}</span>
                {/if}
                <span class="ml-auto flex gap-1.5">
                  {#if s.task_state || s.running}
                    <button
                      class="btn btn-sm btn-danger"
                      onclick={(e) => cancel(s.name, e)}
                      disabled={canceling[s.name]}
                    >
                      {canceling[s.name] ? "Stopping…" : "Stop"}
                    </button>
                  {:else}
                    <button class="btn btn-sm" onclick={(e) => refresh(s.name, e)}>Sync now</button>
                  {/if}
                  <button class="btn btn-sm" onclick={(e) => editSource(s, e)}>Edit</button>
                  <button class="btn btn-sm btn-danger" onclick={(e) => remove(s.name, e)}>Delete</button>
                </span>
              </div>

              <div class="mb-3 flex flex-wrap items-center gap-2">
                <div class="relative min-w-64 flex-1">
                  <span class="pointer-events-none absolute inset-y-0 left-2.5 flex items-center text-faint">
                    <Icon name="search" size={14} />
                  </span>
                  <input
                    class="input w-full pl-8"
                    type="search"
                    placeholder="Search page titles"
                    value={titleFilter[s.name] || ""}
                    oninput={(e) => setTitleFilter(s.name, e.target.value)}
                  />
                </div>
                {#if titleFilter[s.name]}
                  <button class="btn btn-sm" onclick={() => clearTitleFilter(s.name)}>Clear</button>
                  <span class="text-[12px] text-faint">
                    {pageLoading[s.name] ? "Searching…" : `${pageTotal[s.name] || 0} matches`}
                  </span>
                {/if}
              </div>

              {#if s.type === "jira" && teams[s.name]?.length}
                <div class="mb-3 flex items-center gap-2 text-[12px] text-dim">
                  <span>Filter by team:</span>
                  <select
                    class="input max-w-xs"
                    value={teamFilter[s.name] || ""}
                    onchange={(e) => setTeamFilter(s.name, e.target.value)}
                  >
                    <option value="">all teams</option>
                    {#each teams[s.name] as t}
                      <option value={t}>{t}</option>
                    {/each}
                  </select>
                </div>
              {/if}

              {#if !(pages[s.name]?.length)}
                <div class="py-3 text-center text-[13px] text-dim">
                  {pages[s.name]
                    ? titleFilter[s.name]
                      ? "No page titles match this search."
                      : "No pages synced yet."
                    : "Loading…"}
                </div>
              {:else}
                <table class="w-full border-collapse">
                  <tbody>
                    {#each pages[s.name] as p (p.id)}
                      <tr class="hover:bg-surface-2">
                        <td class="border-b border-line px-2 py-1.5">
                          <div class="flex flex-wrap items-center gap-1.5">
                            <button
                              class="cursor-pointer text-left font-mono text-[12.5px] hover:text-accent"
                              onclick={() => navigate(`/sources/${s.name}?doc=${encodeURIComponent(p.slug + ".md")}`)}
                              title={p.url}
                            >
                              {p.title || p.slug}
                            </button>
                            <span
                              class="rounded border border-line px-1.5 py-0.5 text-[10px] text-faint"
                              title={p.url
                                ? `Fetched according to source policy: ${sourceRefreshPolicy(s)}`
                                : "Stored as-is; source refreshes do not overwrite this page"}
                            >
                              {p.url ? "remote · follows source refresh" : "manual · stored as-is"}
                            </span>
                          </div>
                        </td>
                        <td class="border-b border-line px-2 py-1.5"><Status status={p.status} dot /></td>
                        <td class="border-b border-line px-2 py-1.5 text-[11px] text-faint">{fmtDate(p.last_fetch_at)}</td>
                        <td class="border-b border-line px-2 py-1.5 text-right">
                          {#if p.last_error}
                            <span class="mr-2 text-[11px] text-err" title={p.last_error}>fetch failed</span>
                          {/if}
                          {#if p.url}
                            <a class="mr-1 text-[11px] text-dim hover:text-fg" href={p.url} target="_blank" rel="noreferrer noopener">open</a>
                          {/if}
                          {#if s.type === "pages"}
                            <button class="btn btn-sm btn-danger" onclick={() => removePage(s.name, p.slug)}>Remove</button>
                          {/if}
                        </td>
                      </tr>
                    {/each}
                  </tbody>
                </table>

                <!-- Pagination: items are paged server-side so large sources
                     (thousands of pages) load one window at a time. -->
                {#if (pageTotal[s.name] || 0) > PER_PAGE}
                  <div class="mt-2.5 flex items-center justify-between text-[12px] text-dim">
                    <span>{pageRange(s.name)}</span>
                    <div class="flex items-center gap-1">
                      <button
                        class="btn btn-sm"
                        disabled={(pageNum[s.name] || 1) <= 1 || pageLoading[s.name]}
                        onclick={() => goToPage(s.name, 1)}
                        title="First page"
                      >
                        «
                      </button>
                      <button
                        class="btn btn-sm"
                        disabled={(pageNum[s.name] || 1) <= 1 || pageLoading[s.name]}
                        onclick={() => goToPage(s.name, (pageNum[s.name] || 1) - 1)}
                      >
                        Prev
                      </button>
                      <span class="px-1.5 font-mono">
                        {pageNum[s.name] || 1} / {lastPage(s.name)}
                      </span>
                      <button
                        class="btn btn-sm"
                        disabled={!pageHasMore[s.name] || pageLoading[s.name]}
                        onclick={() => goToPage(s.name, (pageNum[s.name] || 1) + 1)}
                      >
                        Next
                      </button>
                      <button
                        class="btn btn-sm"
                        disabled={!pageHasMore[s.name] || pageLoading[s.name]}
                        onclick={() => goToPage(s.name, lastPage(s.name))}
                        title="Last page"
                      >
                        »
                      </button>
                    </div>
                  </div>
                {/if}
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    {/if}
  </div>
{/if}
