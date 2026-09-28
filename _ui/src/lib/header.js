// showGithubLink controls the GitHub link in the header. It is an
// instance-wide preference persisted in the server settings
// (ui_hide_github_link), so it applies to every visitor. The link stays
// visible until the server says otherwise.
import { writable } from "svelte/store";
import { api } from "./api.js";

export const showGithubLink = writable(true);

export async function loadUIPreferences() {
  try {
    const cfg = await api.docsConfig();
    showGithubLink.set(!cfg?.ui_hide_github_link);
  } catch {
    // Settings unavailable: keep the default.
  }
}

// setShowGithubLink applies the change immediately and rolls it back if the
// server rejects it.
export async function setShowGithubLink(show) {
  showGithubLink.set(show);
  try {
    const cfg = await api.setDocsConfig({ ui_hide_github_link: !show });
    showGithubLink.set(!cfg?.ui_hide_github_link);
  } catch (e) {
    showGithubLink.set(!show);
    throw e;
  }
}
