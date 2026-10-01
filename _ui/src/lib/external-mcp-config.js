/** @param {any} connection */
export function createExternalMCPDraft(connection = null) {
  return {
    name: connection?.name || "",
    description: connection?.description || "",
    url: connection?.url || "",
    timeout_seconds: connection?.timeout_seconds || 30,
    disabled: connection?.disabled || false,
    bearer_token: "",
    clear_bearer_token: false,
    headers: (connection?.header_names || []).map((name) => ({ name, value: "" })),
    allowed_tools: [...(connection?.allowed_tools || [])],
    allowed_resources: [...(connection?.allowed_resources || [])],
  };
}

export function identifierList(text) {
  return [...new Set(String(text).split("\n").map((s) => s.trim()).filter(Boolean))].sort();
}

/** @param {any} draft @param {any} saved */
export function buildExternalMCPPayload(draft, saved = null) {
  const changedURL = saved && saved.url !== draft.url.trim();
  const headers = {};
  const names = new Set();
  for (const row of draft.headers) {
    const name = row.name.trim();
    if (!name) throw new Error("Each custom header needs a name.");
    if (names.has(name.toLowerCase())) throw new Error("Custom header names must be unique.");
    names.add(name.toLowerCase());
    // Saved values are tied to the original URL and cannot be reused elsewhere.
    if (changedURL && !row.value) continue;
    Object.defineProperty(headers, name, { value: row.value, enumerable: true });
  }
  return {
    name: draft.name.trim(),
    description: draft.description.trim(),
    url: draft.url.trim(),
    timeout_seconds: Number(draft.timeout_seconds),
    disabled: Boolean(draft.disabled),
    bearer_token: draft.bearer_token,
    clear_bearer_token: Boolean(draft.clear_bearer_token),
    headers,
    allowed_tools: changedURL ? [] : [...draft.allowed_tools],
    allowed_resources: changedURL ? [] : [...draft.allowed_resources],
  };
}
