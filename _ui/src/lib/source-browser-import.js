export function abortImportError() {
  const error = new Error("Import canceled");
  error.name = "AbortError";
  return error;
}

export function buildRelayURL(target, relayValue) {
  const relay = relayValue.trim();
  if (!relay) return "";
  if (relay.includes("{rawUrl}")) return relay.replaceAll("{rawUrl}", target);
  if (relay.includes("{url}")) return relay.replaceAll("{url}", encodeURIComponent(target));
  return relay + encodeURIComponent(target);
}

export function parseSitemapDocument(document, url) {
  const parseError = document.querySelector("parsererror");
  if (parseError) throw new Error(`Invalid sitemap XML at ${url}: ${parseError.textContent.trim()}`);
  const root = document.documentElement?.localName;
  if (root !== "urlset" && root !== "sitemapindex") {
    throw new Error(`Unsupported sitemap root "${root || "unknown"}" at ${url}`);
  }
  const locations = [...document.getElementsByTagNameNS("*", "loc")]
    .map((node) => node.textContent.trim())
    .filter(Boolean)
    .map((location) => new URL(location, url).href);
  return { root, locations };
}

export function parseSitemap(xml, url, DOMParserClass = globalThis.DOMParser) {
  return parseSitemapDocument(new DOMParserClass().parseFromString(xml, "application/xml"), url);
}

export function prepareBrowserHTML(
  html,
  url,
  { DOMParserClass = globalThis.DOMParser, documentTarget = globalThis.document } = {},
) {
  const document = new DOMParserClass().parseFromString(html, "text/html");
  const appShell = !!document.querySelector("#app:empty, #root:empty, #__next:empty, [data-reactroot]:empty");
  document.querySelectorAll("script, style, noscript, template, svg, canvas, iframe").forEach((node) => node.remove());

  const contentRoot = document.querySelector("main, article, [role='main']") || document.body;
  const visibleText = (contentRoot?.textContent || "").replace(/\s+/g, " ").trim();
  if (visibleText.length < 200 || (appShell && visibleText.length < 1000)) {
    throw new Error(
      `${url} appears to be an unrendered JavaScript page (${visibleText.length} visible characters). Use a rendering relay or browser extension.`,
    );
  }

  const compact = documentTarget.implementation.createHTMLDocument(document.title || "");
  compact.body.append(contentRoot.cloneNode(true));
  return compact.documentElement.outerHTML;
}

export function createBrowserImportController({
  importPages,
  getExtensionAvailable,
  getRelay,
  onExtension,
  onImporting,
  onProgress,
  confirmImport,
  fetchTarget = globalThis.fetch,
  windowTarget = globalThis.window,
  locationTarget = globalThis.location,
  cryptoTarget = globalThis.crypto,
  timeoutMs = 60000,
  preparePage = prepareBrowserHTML,
}) {
  const extensionRequests = new Map();
  const imports = new Map();
  let active = false;

  function requireCurrent(operation) {
    if (imports.get(operation.name) !== operation || operation.canceled || operation.controller.signal.aborted) {
      throw abortImportError();
    }
  }

  function cancel(name) {
    const operation = imports.get(name);
    if (!operation) return;
    operation.canceled = true;
    operation.controller.abort();
    if (active) onProgress(name, "Canceling import...");
  }

  function begin(name) {
    cancel(name);
    const operation = { name, controller: new AbortController(), canceled: false };
    imports.set(name, operation);
    onImporting(name, true);
    return operation;
  }

  function finish(operation) {
    if (imports.get(operation.name) !== operation) return;
    imports.delete(operation.name);
    if (!active) return;
    onImporting(operation.name, false);
    onProgress(operation.name, "");
  }

  function handleExtensionMessage(event) {
    if (event.source !== windowTarget || event.origin !== locationTarget.origin) return;
    if (event.data?.type === "KRABBY_EXTENSION_READY") {
      onExtension(true, event.data.version || "unknown");
      return;
    }
    if (event.data?.type !== "KRABBY_EXTENSION_RESPONSE") return;
    const pending = extensionRequests.get(event.data.requestId);
    if (!pending) return;
    extensionRequests.delete(event.data.requestId);
    clearTimeout(pending.timer);
    pending.signal?.removeEventListener("abort", pending.abort);
    if (event.data.error) pending.reject(new Error(event.data.error));
    else pending.resolve(event.data.result);
  }

  function extensionRequest(action, url, signal) {
    if (!getExtensionAvailable()) return Promise.reject(new Error("Krabby browser extension is not connected"));
    if (signal?.aborted) return Promise.reject(abortImportError());
    const requestId = cryptoTarget.randomUUID();
    return new Promise((resolve, reject) => {
      const abort = () => {
        if (!extensionRequests.has(requestId)) return;
        extensionRequests.delete(requestId);
        clearTimeout(timer);
        reject(abortImportError());
      };
      const timer = setTimeout(() => {
        extensionRequests.delete(requestId);
        signal?.removeEventListener("abort", abort);
        reject(new Error(`Browser extension timed out while processing ${url}`));
      }, timeoutMs);
      extensionRequests.set(requestId, { resolve, reject, timer, signal, abort });
      signal?.addEventListener("abort", abort, { once: true });
      windowTarget.postMessage({ type: "KRABBY_EXTENSION_REQUEST", requestId, action, url }, locationTarget.origin);
    });
  }

  async function fetchText(url, signal) {
    let directError;
    try {
      const response = await fetchTarget(url, { credentials: "omit", signal });
      if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
      return await response.text();
    } catch (error) {
      if (error?.name === "AbortError") throw error;
      directError = error;
    }

    if (getExtensionAvailable()) {
      try {
        return await extensionRequest("fetch", url, signal);
      } catch (error) {
        if (error?.name === "AbortError") throw error;
        directError = error;
      }
    }

    const proxied = buildRelayURL(url, getRelay());
    if (!proxied) {
      throw new Error(
        `Browser could not read ${url} (${directError?.message || "CORS blocked"}). Configure a CORS relay below and retry.`,
      );
    }
    const response = await fetchTarget(proxied, { signal });
    if (!response.ok) throw new Error(`CORS relay returned ${response.status} ${response.statusText} for ${url}`);
    return await response.text();
  }

  async function fetchPage(url, signal) {
    if (getExtensionAvailable()) return await extensionRequest("render", url, signal);
    return await fetchText(url, signal);
  }

  async function sitemapURLs(rootURL, operation) {
    const pending = [rootURL];
    const seenSitemaps = new Set();
    const seenPages = new Set();
    while (pending.length) {
      requireCurrent(operation);
      const current = pending.shift();
      if (seenSitemaps.has(current)) continue;
      if (seenSitemaps.size >= 100) throw new Error("Sitemap index exceeds 100 files");
      seenSitemaps.add(current);
      const parsed = parseSitemap(await fetchText(current, operation.controller.signal), current);
      requireCurrent(operation);
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

  async function importPage(name, url) {
    const operation = begin(name);
    onProgress(name, "Fetching page in browser...");
    try {
      const content = preparePage(await fetchPage(url, operation.controller.signal), url);
      requireCurrent(operation);
      onProgress(name, "Converting and indexing...");
      await importPages(name, [{ url, content_type: "text/html", content }], {
        signal: operation.controller.signal,
      });
      requireCurrent(operation);
      return { canceled: false };
    } catch (error) {
      if (error?.name === "AbortError") return { canceled: true };
      throw error;
    } finally {
      finish(operation);
    }
  }

  async function importSitemap(name, url) {
    const operation = begin(name);
    onProgress(name, "Reading sitemap in browser...");
    try {
      const urls = await sitemapURLs(url, operation);
      requireCurrent(operation);
      if (!urls.length) throw new Error("Sitemap contains no page URLs");
      if (urls.length > 200 && !confirmImport(urls.length)) return { canceled: true, declined: true };

      let imported = 0;
      let emptyBatches = 0;
      const failures = [];
      const batchSize = 5;
      for (let offset = 0; offset < urls.length; offset += batchSize) {
        requireCurrent(operation);
        const batchURLs = urls.slice(offset, offset + batchSize);
        onProgress(
          name,
          `Fetching pages ${offset + 1}-${Math.min(offset + batchURLs.length, urls.length)} of ${urls.length}...`,
        );
        const fetched = await Promise.allSettled(
          batchURLs.map(async (pageURL) => ({
            url: pageURL,
            content_type: "text/html",
            content: preparePage(await fetchPage(pageURL, operation.controller.signal), pageURL),
          })),
        );
        requireCurrent(operation);
        const pagesToImport = [];
        fetched.forEach((item, index) => {
          if (item.status === "fulfilled") pagesToImport.push(item.value);
          else failures.push(`${batchURLs[index]}: ${item.reason?.message || "fetch failed"}`);
        });

        let batchImported = 0;
        if (pagesToImport.length) {
          onProgress(name, `Indexing ${imported + 1}-${imported + pagesToImport.length} of ${urls.length}...`);
          try {
            const result = await importPages(name, pagesToImport, { signal: operation.controller.signal });
            requireCurrent(operation);
            batchImported = result?.imported || pagesToImport.length;
          } catch {
            requireCurrent(operation);
            for (const page of pagesToImport) {
              requireCurrent(operation);
              try {
                const result = await importPages(name, [page], { signal: operation.controller.signal });
                requireCurrent(operation);
                batchImported += result?.imported || 1;
              } catch (pageError) {
                if (pageError?.name === "AbortError") throw pageError;
                failures.push(`${page.url}: ${pageError.message}`);
              }
            }
          }
        }

        imported += batchImported;
        if (batchImported > 0) {
          emptyBatches = 0;
        } else {
          emptyBatches += 1;
          const detail = failures.slice(-3).join("; ");
          if (imported === 0 || emptyBatches >= 3) {
            throw new Error(`No pages could be imported; stopped early. ${detail}`);
          }
        }
        requireCurrent(operation);
      }
      return { canceled: false, imported, failures };
    } catch (error) {
      if (error?.name === "AbortError") return { canceled: true, declined: false };
      throw error;
    } finally {
      finish(operation);
    }
  }

  function start() {
    if (active) return;
    active = true;
    windowTarget.addEventListener("message", handleExtensionMessage);
    windowTarget.postMessage({ type: "KRABBY_EXTENSION_PING" }, locationTarget.origin);
  }

  function stop() {
    active = false;
    for (const operation of imports.values()) {
      operation.canceled = true;
      operation.controller.abort();
    }
    imports.clear();
    windowTarget.removeEventListener("message", handleExtensionMessage);
    for (const pending of extensionRequests.values()) {
      clearTimeout(pending.timer);
      pending.signal?.removeEventListener("abort", pending.abort);
      pending.reject(new Error("Page closed"));
    }
    extensionRequests.clear();
  }

  return { cancel, importPage, importSitemap, start, stop };
}
