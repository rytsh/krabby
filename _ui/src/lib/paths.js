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

// buildOwnerTree turns the flat list of owner groups ([{ owner, count }])
// returned by the server into a nested folder tree so that, e.g.,
// ".../parser" and ".../parser/poc" render as parent/child instead of two
// siblings. Every "/" segment of an owner path becomes a folder node; a node
// is a real (loadable) owner group when it appears in the input list (it then
// carries `owner` + `count`), while purely intermediate segments are folders
// with no repos of their own. A single node can be both — e.g. "parser" may
// own repos directly and still contain a "poc" subgroup.
//
// Returns an array of root nodes, each shaped as:
//   { key, label, owner|null, count, children: Node[] }
// where `key` is the full path to that node, `label` its last segment, and
// `owner` the group key to pass to loadOwnerRepos (null for folder-only nodes).
export function buildOwnerTree(groups) {
  const root = { children: new Map() };

  const ensure = (node, segs) => {
    let cur = node;
    let path = "";
    for (const seg of segs) {
      path = path ? `${path}/${seg}` : seg;
      let child = cur.children.get(seg);
      if (!child) {
        child = { key: path, label: seg, owner: null, count: 0, children: new Map() };
        cur.children.set(seg, child);
      }
      cur = child;
    }
    return cur;
  };

  for (const g of groups || []) {
    const owner = g.owner || "";
    // Bare-name repos (no "/") group under the synthetic root key "".
    const segs = owner === "" ? [""] : owner.split("/");
    const node = ensure(root, segs);
    node.owner = owner;
    node.count = g.count || 0;
  }

  const toArray = (node) =>
    [...node.children.values()]
      .sort((a, b) => a.label.localeCompare(b.label))
      .map((n) => ({
        key: n.key,
        label: n.label === "" ? "(root)" : n.label,
        owner: n.owner,
        count: n.count,
        children: toArray(n),
      }));

  return toArray(root);
}

// collapseTree merges chains of folder-only nodes that have a single child
// into one node, so long shared prefixes (host/org/team/...) don't waste a
// level of indentation each. A node is collapsed into its lone child when it
// owns no repos itself; the labels are then joined with "/". The branch point
// where paths actually diverge (e.g. "parser", which owns repos AND has the
// "poc" subgroup) is always preserved.
export function collapseTree(nodes) {
  return (nodes || []).map((n) => {
    let node = { ...n, children: collapseTree(n.children) };
    while (node.owner === null && node.children.length === 1) {
      const child = node.children[0];
      node = {
        key: child.key,
        label: `${node.label}/${child.label}`,
        owner: child.owner,
        count: child.count,
        children: child.children,
      };
    }
    return node;
  });
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
