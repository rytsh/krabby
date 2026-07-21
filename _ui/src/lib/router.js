import { writable } from "svelte/store";

// Minimal History-API router. path is the current pathname; navigate() pushes
// state and updates the store. Anchor clicks with data-link are intercepted.
export const path = writable(window.location.pathname);

export function navigate(to) {
  if (to === window.location.pathname) return;
  window.history.pushState({}, "", to);
  path.set(to);
}

window.addEventListener("popstate", () => {
  path.set(window.location.pathname);
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
