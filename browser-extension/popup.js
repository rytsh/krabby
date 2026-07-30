const originLabel = document.querySelector("#origin");
const toggle = document.querySelector("#toggle");

let currentOrigin = "";
let connected = false;

async function load() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  try {
    const url = new URL(tab.url);
    if (url.protocol !== "http:" && url.protocol !== "https:") throw new Error();
    currentOrigin = url.origin;
  } catch {
    originLabel.textContent = "Open an HTTP(S) Krabby UI tab first.";
    return;
  }

  const { krabbyOrigins = [] } = await chrome.storage.local.get("krabbyOrigins");
  connected = krabbyOrigins.includes(currentOrigin);
  originLabel.textContent = currentOrigin;
  toggle.textContent = connected ? "Disconnect this origin" : "Connect this origin";
  toggle.disabled = false;
}

toggle.addEventListener("click", async () => {
  const { krabbyOrigins = [] } = await chrome.storage.local.get("krabbyOrigins");
  const origins = new Set(krabbyOrigins);
  if (connected) origins.delete(currentOrigin);
  else origins.add(currentOrigin);
  await chrome.storage.local.set({ krabbyOrigins: [...origins] });
  window.close();
});

load();
