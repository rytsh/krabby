import assert from "node:assert/strict";
import test from "node:test";

import {
  buildRelayURL,
  createBrowserImportController,
  parseSitemap,
  parseSitemapDocument,
  prepareBrowserHTML,
} from "./source-browser-import.js";

test("relay URLs support encoded, raw, and prefix templates", () => {
  const target = "https://docs.example/a page?q=one";
  assert.equal(buildRelayURL(target, "https://relay/?url={url}"), `https://relay/?url=${encodeURIComponent(target)}`);
  assert.equal(buildRelayURL(target, "https://relay/?url={rawUrl}"), `https://relay/?url=${target}`);
  assert.equal(buildRelayURL(target, "https://relay/"), `https://relay/${encodeURIComponent(target)}`);
  assert.equal(buildRelayURL(target, "  "), "");
});

test("sitemap parser accepts indexes and resolves relative locations", () => {
  class FakeDOMParser {
    parseFromString(xml, type) {
      assert.equal(xml, "<sitemap />");
      assert.equal(type, "application/xml");
      return {
        documentElement: { localName: "sitemapindex" },
        querySelector: () => null,
        getElementsByTagNameNS: () => [{ textContent: " child.xml " }, { textContent: "https://cdn.example/a.xml" }],
      };
    }
  }

  assert.deepEqual(parseSitemap("<sitemap />", "https://docs.example/sitemap.xml", FakeDOMParser), {
    root: "sitemapindex",
    locations: ["https://docs.example/child.xml", "https://cdn.example/a.xml"],
  });
});

test("sitemap parser reports XML and unsupported-root errors", () => {
  assert.throws(
    () =>
      parseSitemapDocument(
        {
          querySelector: () => ({ textContent: " malformed XML " }),
          documentElement: { localName: "urlset" },
        },
        "https://docs.example/sitemap.xml",
      ),
    /Invalid sitemap XML.*malformed XML/,
  );
  assert.throws(
    () =>
      parseSitemapDocument(
        {
          querySelector: () => null,
          documentElement: { localName: "rss" },
        },
        "https://docs.example/feed.xml",
      ),
    /Unsupported sitemap root "rss"/,
  );
});

test("browser HTML preparation strips chrome and keeps the content root", () => {
  let removed = 0;
  const contentRoot = {
    textContent: "useful content ".repeat(20),
    cloneNode: (deep) => ({ cloned: deep }),
  };
  class FakeDOMParser {
    parseFromString(html, type) {
      assert.equal(html, "<html />");
      assert.equal(type, "text/html");
      return {
        title: "Docs",
        body: contentRoot,
        querySelector: (selector) => (selector.startsWith("#app") ? null : contentRoot),
        querySelectorAll: () => [{ remove: () => (removed += 1) }, { remove: () => (removed += 1) }],
      };
    }
  }
  const documentTarget = {
    implementation: {
      createHTMLDocument: (title) => {
        assert.equal(title, "Docs");
        return {
          body: { append: (node) => assert.deepEqual(node, { cloned: true }) },
          documentElement: { outerHTML: "<html><body>compact</body></html>" },
        };
      },
    },
  };

  assert.equal(
    prepareBrowserHTML("<html />", "https://docs.example/page", { DOMParserClass: FakeDOMParser, documentTarget }),
    "<html><body>compact</body></html>",
  );
  assert.equal(removed, 2);
});

test("browser import forwards its signal and cancel aborts an in-flight backend import", async () => {
  let importSignal;
  let importStarted;
  const started = new Promise((resolve) => (importStarted = resolve));
  const controller = createBrowserImportController({
    importPages: (_name, _pages, { signal }) => {
      importSignal = signal;
      importStarted();
      return new Promise((resolve, reject) => {
        signal.addEventListener(
          "abort",
          () => {
            const error = new Error("aborted");
            error.name = "AbortError";
            reject(error);
          },
          { once: true },
        );
      });
    },
    getExtensionAvailable: () => false,
    getRelay: () => "",
    onExtension: () => {},
    onImporting: () => {},
    onProgress: () => {},
    confirmImport: () => true,
    fetchTarget: async (_url, { signal }) => {
      assert.equal(signal.aborted, false);
      return { ok: true, text: async () => "page" };
    },
    preparePage: () => "<html>prepared</html>",
  });

  const importing = controller.importPage("docs", "https://docs.example/page");
  await started;
  assert.ok(importSignal instanceof AbortSignal);
  assert.equal(importSignal.aborted, false);

  controller.cancel("docs");

  assert.equal(importSignal.aborted, true);
  assert.deepEqual(await importing, { canceled: true });
});
