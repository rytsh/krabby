import { writable } from "svelte/store";

// Minimal History-API router. path is the current pathname plus query string;
// navigate() pushes state and updates the store. Anchor clicks with use:link
// are intercepted.
export const path = writable(window.location.pathname + window.location.search);

export function navigate(to) {
  if (to === window.location.pathname + window.location.search) return;
  window.history.pushState({}, "", to);
  path.set(to);
}

window.addEventListener("popstate", () => {
  path.set(window.location.pathname + window.location.search);
});

// Use as: <a href="/repos" use:link>. Intercepts same-origin navigation.
export function link(node) {
  function onClick(e) {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const href = node.getAttribute("href");
    if (!href || href.startsWith("http")) return;
    e.preventDefault();
    navigate(href);
  }

  node.addEventListener("click", onClick);
  return {
    destroy() {
      node.removeEventListener("click", onClick);
    },
  };
}
