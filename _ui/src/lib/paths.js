// Repo ids are full paths (host/group/.../name), which keeps repositories on
// different git servers or in different (nested) groups from colliding, but
// makes for long sidebar labels. shortLabels computes a compact display label
// per group: each path starts at the last segment it shares with the group it
// has the longest common prefix with, so unique prefixes disappear while one
// shared parent segment stays as context.
//
//   github.com/a/b/c/d, github.com/a/b/c  ->  c/d, c
//   ...plus github.com/b/x                ->  github.com/b/x (nothing shared)
//
// The full path is always available via tooltips; users can also disable
// shortening entirely from the settings page (sidebarPathMode = "full").
import { writable } from "svelte/store";

// dirOf returns everything before the last "/" ("" for bare names).
export function dirOf(id) {
  const idx = id.lastIndexOf("/");
  return idx > 0 ? id.slice(0, idx) : "";
}

// nameOf returns the last path segment (the repository name).
export function nameOf(id) {
  const idx = id.lastIndexOf("/");
  return idx >= 0 ? id.slice(idx + 1) : id;
}

// shortLabels maps each key (full group path) to its shortened display label.
export function shortLabels(keys) {
  const segs = keys.map((k) => k.split("/"));

  // For every path, find the longest common prefix (in segments) with any
  // other path, then start the label one segment before the divergence so the
  // last shared segment is kept as context. Paths sharing nothing stay full.
  const starts = segs.map((a, i) => {
    let maxLcp = 0;
    segs.forEach((b, j) => {
      if (i === j) return;
      let l = 0;
      while (l < a.length && l < b.length && a[l] === b[l]) l++;
      if (l > maxLcp) maxLcp = l;
    });

    // A lone group needs no context: show just its last segment.
    const start = keys.length === 1 ? a.length - 1 : Math.max(0, maxLcp - 1);
    return Math.min(start, a.length - 1);
  });

  // Unrelated prefixes can still shorten to the same label (a/m/x and b/m/x
  // both -> "m/x"); extend duplicates leftwards until every label is unique.
  for (;;) {
    const byLabel = new Map();
    keys.forEach((_, i) => {
      const label = segs[i].slice(starts[i]).join("/");
      if (!byLabel.has(label)) byLabel.set(label, []);
      byLabel.get(label).push(i);
    });

    let changed = false;
    for (const idxs of byLabel.values()) {
      if (idxs.length < 2) continue;
      for (const i of idxs) {
        if (starts[i] > 0) {
          starts[i] -= 1;
          changed = true;
        }
      }
    }

    if (!changed) break;
  }

  const out = {};
  keys.forEach((k, i) => {
    out[k] = segs[i].slice(starts[i]).join("/");
  });
  return out;
}

// sidebarPathMode controls how the sidebar renders repo paths: "smart"
// (shortened, default) or "full". A pure display preference, persisted per
// browser in localStorage and configurable from the settings page.
const PATH_MODE_KEY = "krabby-sidebar-paths";

export const sidebarPathMode = writable(localStorage.getItem(PATH_MODE_KEY) === "full" ? "full" : "smart");

sidebarPathMode.subscribe((mode) => {
  try {
    localStorage.setItem(PATH_MODE_KEY, mode);
  } catch {
    // Private mode / storage full: the preference just won't persist.
  }
});
