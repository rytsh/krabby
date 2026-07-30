const ALLOWED_PROTOCOLS = new Set(["http:", "https:"]);

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (message?.type !== "krabby-import") return false;
  handleImport(message, sender).then(
    (result) => sendResponse({ result }),
    (error) => sendResponse({ error: error?.message || String(error) }),
  );
  return true;
});

async function handleImport(message, sender) {
  await assertConnectedKrabby(sender);
  const target = new URL(message.url);
  if (!ALLOWED_PROTOCOLS.has(target.protocol)) throw new Error("Only HTTP(S) URLs can be imported");

  if (message.action === "fetch") {
    const response = await fetch(target.href, { credentials: "include", cache: "no-cache" });
    if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
    return await response.text();
  }
  if (message.action === "render") return await renderPage(target.href);
  throw new Error(`Unknown Krabby extension action: ${message.action}`);
}

async function assertConnectedKrabby(sender) {
  if (!sender.url) throw new Error("Missing requesting page URL");
  const origin = new URL(sender.url).origin;
  const { krabbyOrigins = [] } = await chrome.storage.local.get("krabbyOrigins");
  if (!krabbyOrigins.includes(origin)) {
    throw new Error(`Krabby origin is not connected: ${origin}`);
  }
}

async function renderPage(url) {
  const tab = await chrome.tabs.create({ url, active: false });
  try {
    await waitForTab(tab.id, 30000);
    await waitForStableText(tab.id, 20000);
    const [{ result }] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: captureReadableHTML,
    });
    if (!result?.html) throw new Error(`Rendered page returned no HTML: ${url}`);
    return result.html;
  } finally {
    await chrome.tabs.remove(tab.id).catch(() => {});
  }
}

async function waitForTab(tabId, timeoutMs) {
  const tab = await chrome.tabs.get(tabId);
  if (tab.status === "complete") return;

  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => finish(new Error("Timed out waiting for page load")), timeoutMs);
    const listener = (updatedId, change) => {
      if (updatedId === tabId && change.status === "complete") finish();
    };
    function finish(error) {
      clearTimeout(timer);
      chrome.tabs.onUpdated.removeListener(listener);
      error ? reject(error) : resolve();
    }
    chrome.tabs.onUpdated.addListener(listener);
  });
}

async function waitForStableText(tabId, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let previous = -1;
  let stable = 0;
  while (Date.now() < deadline) {
    const [{ result = 0 }] = await chrome.scripting.executeScript({
      target: { tabId },
      func: () => (document.body?.innerText || "").trim().length,
    });
    if (result >= 200 && Math.abs(result - previous) < 5) stable++;
    else stable = 0;
    if (stable >= 3) return;
    previous = result;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
}

function captureReadableHTML() {
  const clone = document.documentElement.cloneNode(true);
  clone.querySelectorAll("script, style, noscript, template, svg, canvas, iframe").forEach((node) => node.remove());
  const content = clone.querySelector("main, article, [role='main']") || clone.querySelector("body");
  if (!content) return { html: "" };

  const output = document.implementation.createHTMLDocument(document.title || "");
  output.body.append(content.cloneNode(true));
  return {
    html: output.documentElement.outerHTML,
    textLength: (content.textContent || "").replace(/\s+/g, " ").trim().length,
  };
}
