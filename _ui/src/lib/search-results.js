// Accept the docs envelope and legacy bare arrays during rolling upgrades.
export function docsSearchPage(response, requestedMode) {
  return {
    results: Array.isArray(response) ? response : response?.results || [],
    mode: response?.mode || requestedMode,
    note: response?.note || "",
  };
}
