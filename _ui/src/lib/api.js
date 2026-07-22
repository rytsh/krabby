// Thin fetch wrapper for the krabby REST API. All paths are relative so the
// same build works whether served from the embedded server or the dev proxy.
import { errorToast } from "./toast.js";

const BASE = "/api/v1";

async function req(path, opts = {}) {
  try {
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
  repo: (id) => req(`/repos/${id}`),
  addRepo: (url, branch) =>
    req("/repos", { method: "POST", keepalive: true, body: JSON.stringify({ url, branch: branch || "" }) }),
  deleteRepo: (id) => req(`/repos/${id}`, { method: "DELETE", keepalive: true }),
  refreshRepo: (id) => req(`/repos/${id}/refresh`, { method: "POST", keepalive: true }),
  cancelRepoJob: (id) => req(`/repos/${id}/cancel`, { method: "POST", keepalive: true }),
  generate: (id, targets) =>
    req(`/repos/${id}/generate`, { method: "POST", keepalive: true, body: JSON.stringify({ targets }) }),
  lockStatus: (id) => req(`/repos/${id}/lock`),
  files: (id, subdir = "", recursive = false) =>
    req(`/repos/${id}/files?subdir=${encodeURIComponent(subdir)}&recursive=${recursive}`),
  file: (id, path) => req(`/repos/${id}/file?path=${encodeURIComponent(path)}`),
  credentials: () => req("/credentials"),
  mcpKey: () => req("/mcp/api-key"),
  setMcpKey: (apiKey) => req("/mcp/api-key", { method: "PUT", body: JSON.stringify({ api_key: apiKey }) }),
  clearMcpKey: () => req("/mcp/api-key", { method: "DELETE" }),
  docsConfig: () => req("/docs/config"),
  setDocsConfig: (cfg) => req("/docs/config", { method: "PUT", body: JSON.stringify(cfg) }),
  testLLM: (cfg) => req("/docs/config/test/llm", { method: "POST", body: JSON.stringify(cfg) }),
  testEmbedder: (cfg) => req("/docs/config/test/embedder", { method: "POST", body: JSON.stringify(cfg) }),
  testCodeEmbedder: (cfg) => req("/docs/config/test/code-embedder", { method: "POST", body: JSON.stringify(cfg) }),
  searchDocs: (q, repo = "", top = 0) =>
    req(`/docs/search?q=${encodeURIComponent(q)}&repo=${encodeURIComponent(repo)}&top=${top}`),
  searchCode: (q, repo = "", mode = "normal", page = 1, perPage = 20, top = 0) =>
    req(
      `/code/search?q=${encodeURIComponent(q)}&repo=${encodeURIComponent(repo)}&mode=${mode}&page=${page}&per_page=${perPage}&top=${top}`,
    ),
  docs: (id) => req(`/repos/${id}/docs`),
  doc: (id, path) => req(`/repos/${id}/doc?path=${encodeURIComponent(path)}`),
};
