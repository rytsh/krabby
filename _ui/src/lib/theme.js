// Theme store: persists the user's choice in localStorage and falls back to the
// OS preference. Applies the theme by setting data-theme on <html> so the CSS
// variable palettes in app.css take effect.
import { writable } from "svelte/store";

const KEY = "krabby-theme";

function initial() {
  const saved = localStorage.getItem(KEY);
  if (saved === "light" || saved === "dark") return saved;
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

function apply(value) {
  document.documentElement.setAttribute("data-theme", value);
}

// Apply immediately (before first paint) to avoid a flash of the wrong theme.
const start = initial();
apply(start);

export const theme = writable(start);

let first = true;
theme.subscribe((value) => {
  apply(value);
  if (first) {
    first = false;
    return; // don't animate the initial application
  }
  localStorage.setItem(KEY, value);
  // Briefly enable color transitions only around an explicit toggle.
  const el = document.documentElement;
  el.classList.add("theme-anim");
  window.setTimeout(() => el.classList.remove("theme-anim"), 220);
});

export function toggleTheme() {
  theme.update((v) => (v === "dark" ? "light" : "dark"));
}
