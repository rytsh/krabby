// Thin fetch wrapper for the krabby REST API. All paths are relative so the
// same build works whether served from the embedded server or the dev proxy.
const BASE = "/api/v1";

async function req(path, opts = {}) {
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
}

export const api = {
  settings: () => req("/settings"),
  repos: () => req("/repos"),
  repo: (id) => req(`/repos/${id}`),
  addRepo: (url, branch) =>
    req("/repos", { method: "POST", body: JSON.stringify({ url, branch: branch || "" }) }),
  deleteRepo: (id) => req(`/repos/${id}`, { method: "DELETE" }),
  refreshRepo: (id) => req(`/repos/${id}/refresh`, { method: "POST" }),
  lockStatus: (id) => req(`/repos/${id}/lock`),
  files: (id, subdir = "", recursive = false) =>
    req(`/repos/${id}/files?subdir=${encodeURIComponent(subdir)}&recursive=${recursive}`),
  file: (id, path) => req(`/repos/${id}/file?path=${encodeURIComponent(path)}`),
  credentials: () => req("/credentials"),
  docsConfig: () => req("/docs/config"),
  setDocsConfig: (cfg) => req("/docs/config", { method: "PUT", body: JSON.stringify(cfg) }),
  testLLM: (cfg) => req("/docs/config/test/llm", { method: "POST", body: JSON.stringify(cfg) }),
  testEmbedder: (cfg) => req("/docs/config/test/embedder", { method: "POST", body: JSON.stringify(cfg) }),
  testCodeEmbedder: (cfg) => req("/docs/config/test/code-embedder", { method: "POST", body: JSON.stringify(cfg) }),
  searchDocs: (q, repo = "", top = 0) =>
    req(`/docs/search?q=${encodeURIComponent(q)}&repo=${encodeURIComponent(repo)}&top=${top}`),
  searchCode: (q, repo = "", top = 0) =>
    req(`/code/search?q=${encodeURIComponent(q)}&repo=${encodeURIComponent(repo)}&top=${top}`),
  docs: (id) => req(`/repos/${id}/docs`),
  doc: (id, path) => req(`/repos/${id}/doc?path=${encodeURIComponent(path)}`),
};
