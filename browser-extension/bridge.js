const PAGE_REQUEST = "KRABBY_EXTENSION_REQUEST";
const PAGE_RESPONSE = "KRABBY_EXTENSION_RESPONSE";
const PAGE_READY = "KRABBY_EXTENSION_READY";
const PAGE_PING = "KRABBY_EXTENSION_PING";

let enabled = false;

function announceReady() {
  const manifest = chrome.runtime.getManifest();
  window.postMessage({ type: PAGE_READY, version: manifest.version_name || manifest.version }, location.origin);
}

async function refreshEnabled() {
  const { krabbyOrigins = [] } = await chrome.storage.local.get("krabbyOrigins");
  const next = krabbyOrigins.includes(location.origin);
  if (next && !enabled) announceReady();
  enabled = next;
}

refreshEnabled();
chrome.storage.onChanged.addListener((changes, area) => {
  if (area === "local" && changes.krabbyOrigins) refreshEnabled();
});

window.addEventListener("message", async (event) => {
  if (!enabled || event.source !== window || event.origin !== location.origin) return;
  const message = event.data;
  if (message?.type === PAGE_PING) {
    announceReady();
    return;
  }
  if (!message || message.type !== PAGE_REQUEST || !message.requestId) return;

  try {
    const result = await chrome.runtime.sendMessage({
      type: "krabby-import",
      action: message.action,
      url: message.url,
    });
    window.postMessage({ type: PAGE_RESPONSE, requestId: message.requestId, ...result }, location.origin);
  } catch (error) {
    window.postMessage(
      { type: PAGE_RESPONSE, requestId: message.requestId, error: error?.message || String(error) },
      location.origin,
    );
  }
});
