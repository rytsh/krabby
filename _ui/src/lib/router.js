// Hash-based routing built on svelte-spa-router. The app is served as a single
// index.html (possibly under a server base path like /krabby/), so routing lives
// entirely in the URL fragment: "/krabby/#/repos/owner/name". Using the hash
// keeps the document URL — and therefore every relative asset/API request —
// anchored at the base path, so the same build works at the root or under any
// prefix with no base-path awareness in the client.
//
// This module adapts svelte-spa-router to the small API the app already uses:
//   - path: a readable store of the current route (location + querystring),
//     always starting with '/'.
//   - navigate(to): push a new route onto the history.
//   - link: the svelte-spa-router action for <a href="/repos" use:link>.
import { readable } from "svelte/store";
import { push, link } from "svelte-spa-router";

export { link };

function currentPath() {
  const h = window.location.hash.slice(1); // drop leading '#'
  return h.startsWith("/") ? h : "/" + (h || "");
}

// Mirror svelte-spa-router's hash into a Svelte store. The library dispatches a
// hashchange on every push/replace, so listening here keeps us in sync.
export const path = readable(currentPath(), (set) => {
  const update = () => set(currentPath());
  window.addEventListener("hashchange", update);
  return () => window.removeEventListener("hashchange", update);
});

// Normalize a bare load (no hash) to "#/" so relative links resolve cleanly.
if (typeof window !== "undefined" && !window.location.hash) {
  window.history.replaceState(
    {},
    "",
    window.location.pathname + window.location.search + "#/",
  );
}

// navigate delegates to svelte-spa-router's push. It accepts app-absolute paths
// ("/repos/..."), matching the previous router's contract.
export function navigate(to) {
  const next = to.startsWith("/") || to.startsWith("#/") ? to : "/" + to;
  push(next);
}
