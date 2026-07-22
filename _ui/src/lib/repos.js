// Shared repository list store. The sidebar and the Repos page both consume
// this so a single fetch/poll keeps them in sync.
import { writable, derived } from "svelte/store";
import { api } from "./api.js";

export const repos = writable([]);
export const reposError = writable("");
export const reposLoaded = writable(false);

export async function loadRepos() {
  try {
    const list = await api.repos();
    repos.set(Array.isArray(list) ? list : []);
    reposError.set("");
  } catch (e) {
    reposError.set(e.message);
  } finally {
    reposLoaded.set(true);
  }
}

// Repos grouped by path prefix (owner), sorted alphabetically on both levels.
// { owner: string, repos: Repo[] }[]
export const repoGroups = derived(repos, ($repos) => {
  const groups = new Map();
  for (const r of $repos) {
    const idx = r.id.indexOf("/");
    const owner = idx > 0 ? r.id.slice(0, idx) : "";
    if (!groups.has(owner)) groups.set(owner, []);
    groups.get(owner).push(r);
  }
  return [...groups.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([owner, list]) => ({
      owner,
      repos: list.sort((a, b) => a.id.localeCompare(b.id)),
    }));
});
