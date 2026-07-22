// Thin fetch wrapper for the krabby REST API. The base is relative ("api/v1")
// so it resolves against the document URL, which the hash router keeps anchored
// at the server base path (e.g. /krabby/). The same build therefore works at
// the root or under any prefix, and via the dev proxy.
import { errorToast } from "./toast.js";

const BASE = "api/v1";

async function req(path, opts = {}) {
  try {
    // path starts with "/"; joining onto the relative BASE keeps it relative.
    const res = await fetch(BASE + path, {
      headers: { "Content-Type": "application/json" },
      ...opts,
    });

    const text = await res.text();
    let body = null;
    if (text) {
      try {
        body = JSON.parse(text);
      } catch {
        body = text;
      }
    }

    if (!res.ok) {
      const msg = body && body.error ? body.error : `${res.status} ${res.statusText}`;
      throw new Error(msg);
    }

    return body;
  } catch (error) {
    errorToast(error);
    throw error;
  }
}

export const api = {
  settings: () => req("/settings"),
  // repos returns a paginated envelope: { items, total, page, per_page }.
  // opts: { page, perPage, q, owner }.
  repos: ({ page = 1, perPage = 20, q = "", owner = "" } = {}) => {
    const p = new URLSearchParams();
    if (page) p.set("page", page);
    if (perPage) p.set("per_page", perPage);
    if (q) p.set("q", q);
    if (owner) p.set("owner", owner);
    return req(`/repos?${p.toString()}`);
  },
  // owners returns [{ owner, count }] for the sidebar tree.
  owners: () => req("/repos/owners"),
  // activeRepos returns only repos with running jobs: [{ id, running, status }].
  activeRepos: () => req("/repos/active"),
  // Repo ids are full paths (host/group/.../name) with any number of "/"
  // segments, so repo actions use a GitLab-style "/-/" separator between the
  // id and the action.
  repo: (id) => req(`/repos/${id}`),
  addRepo: (url, branch) =>
    req("/repos", { method: "POST", keepalive: true, body: JSON.stringify({ url, branch: branch || "" }) }),
  deleteRepo: (id) => req(`/repos/${id}`, { method: "DELETE", keepalive: true }),
  refreshRepo: (id) => req(`/repos/${id}/-/refresh`, { method: "POST", keepalive: true }),
  cancelRepoJob: (id) => req(`/repos/${id}/-/cancel`, { method: "POST", keepalive: true }),
  generate: (id, targets) =>
    req(`/repos/${id}/-/generate`, { method: "POST", keepalive: true, body: JSON.stringify({ targets }) }),
  lockStatus: (id) => req(`/repos/${id}/-/lock`),
  files: (id, subdir = "", recursive = false) =>
    req(`/repos/${id}/-/files?subdir=${encodeURIComponent(subdir)}&recursive=${recursive}`),
  file: (id, path) => req(`/repos/${id}/-/file?path=${encodeURIComponent(path)}`),
  credentials: () => req("/credentials"),
  setCredential: (credential) =>
    req("/credentials", { method: "PUT", body: JSON.stringify(credential) }),
  deleteCredential: (pattern) =>
    req(`/credentials?pattern=${encodeURIComponent(pattern)}`, { method: "DELETE" }),
  mcpKey: () => req("/mcp/api-key"),
  setMcpKey: (apiKey) => req("/mcp/api-key", { method: "PUT", body: JSON.stringify({ api_key: apiKey }) }),
  clearMcpKey: () => req("/mcp/api-key", { method: "DELETE" }),
  docsConfig: () => req("/docs/config"),
  setDocsConfig: (cfg) => req("/docs/config", { method: "PUT", body: JSON.stringify(cfg) }),
  testLLM: (cfg) => req("/docs/config/test/llm", { method: "POST", body: JSON.stringify(cfg) }),
  testEmbedder: (cfg) => req("/docs/config/test/embedder", { method: "POST", body: JSON.stringify(cfg) }),
  testCodeEmbedder: (cfg) => req("/docs/config/test/code-embedder", { method: "POST", body: JSON.stringify(cfg) }),
  // searchDocs scoping: repo may be a repository id or a web-source key
  // ("web:<name>") and wins over scope; scope is all|repos|sources.
  searchDocs: (q, repo = "", top = 0, scope = "") =>
    req(
      `/docs/search?q=${encodeURIComponent(q)}&repo=${encodeURIComponent(repo)}&top=${top}&scope=${encodeURIComponent(scope)}`,
    ),
  searchCode: (q, repo = "", mode = "normal", page = 1, perPage = 20, top = 0) =>
    req(
      `/code/search?q=${encodeURIComponent(q)}&repo=${encodeURIComponent(repo)}&mode=${mode}&page=${page}&per_page=${perPage}&top=${top}`,
    ),
  docs: (id) => req(`/repos/${id}/-/docs`),
  doc: (id, path) => req(`/repos/${id}/-/doc?path=${encodeURIComponent(path)}`),
  // Web content sources (wikis, Confluence spaces).
  sources: () => req("/sources"),
  addSource: (body) => req("/sources", { method: "POST", keepalive: true, body: JSON.stringify(body) }),
  source: (name) => req(`/sources/${name}`),
  updateSource: (name, body) => req(`/sources/${name}`, { method: "PUT", body: JSON.stringify(body) }),
  deleteSource: (name) => req(`/sources/${name}`, { method: "DELETE", keepalive: true }),
  refreshSource: (name) => req(`/sources/${name}/refresh`, { method: "POST", keepalive: true }),
  addSourcePage: (name, url) =>
    req(`/sources/${name}/pages`, { method: "POST", keepalive: true, body: JSON.stringify({ url }) }),
  deleteSourcePage: (name, slug) =>
    req(`/sources/${name}/pages?slug=${encodeURIComponent(slug)}`, { method: "DELETE", keepalive: true }),
  sourceDoc: (name, path) => req(`/sources/${name}/doc?path=${encodeURIComponent(path)}`),
};
