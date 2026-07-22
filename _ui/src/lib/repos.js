// Shared repository stores. The sidebar shows owner groups lazily: the top
// level (owners + counts) is fetched once, and each owner's repos are fetched
// on demand when its group is expanded, then cached. This keeps the UI cheap
// even with a very large number of tracked repositories.
import { writable } from "svelte/store";
import { api } from "./api.js";

// Owner groups for the sidebar tree: [{ owner, count }].
export const owners = writable([]);
export const ownersError = writable("");
export const ownersLoaded = writable(false);

// Lazily-loaded repos per owner: { [owner]: Repo[] }.
export const ownerRepos = writable({});
// Owners whose repos are currently being fetched: Set<string>.
export const ownerLoading = writable(new Set());

let ownerReposCache = {};
ownerRepos.subscribe((v) => (ownerReposCache = v));

// loadOwners fetches the owner list for the sidebar. Cheap: ids only.
export async function loadOwners() {
  try {
    const list = await api.owners();
    owners.set(Array.isArray(list) ? list : []);
    ownersError.set("");
  } catch (e) {
    ownersError.set(e.message);
  } finally {
    ownersLoaded.set(true);
  }
}

// loadOwnerRepos fetches (and caches) the repos of a single owner. Pass
// force=true to bypass the cache after a mutation (add/remove/refresh).
export async function loadOwnerRepos(owner, force = false) {
  if (!force && ownerReposCache[owner]) return ownerReposCache[owner];

  ownerLoading.update((s) => new Set(s).add(owner));
  try {
    // per_page 0 is clamped server-side; ask for a generous page so a single
    // owner's repos come back in one request. Owners with more than this are
    // still correct (extra repos just won't show until refined), but in
    // practice a single owner rarely exceeds this.
    const res = await api.repos({ owner, page: 1, perPage: 200 });
    const items = res?.items || [];
    ownerRepos.update((m) => ({ ...m, [owner]: items }));
    return items;
  } finally {
    ownerLoading.update((s) => {
      const next = new Set(s);
      next.delete(owner);
      return next;
    });
  }
}

// invalidateOwners refetches the owner list and any already-loaded owner
// repos. Call after add/remove/refresh so the sidebar reflects the change.
export async function invalidateOwners() {
  await loadOwners();
  const loaded = Object.keys(ownerReposCache);
  await Promise.all(loaded.map((owner) => loadOwnerRepos(owner, true)));
}

// ownerOf returns the owner prefix of a repo id ("" for root-level ids).
export function ownerOf(id) {
  const idx = id.indexOf("/");
  return idx > 0 ? id.slice(0, idx) : "";
}
