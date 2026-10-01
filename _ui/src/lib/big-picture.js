/** @param {any} picture */
export function createPictureDraft(picture = null) {
  return {
    name: picture?.name || "",
    title: picture?.title || "",
    namespace: picture?.namespace || "default",
    description: picture?.description || "",
    prompt: picture?.prompt || "",
    sources: (picture?.sources || []).map((source) => ({ ...source })),
    expected_version: picture?.version || 0,
    schedule_text: (picture?.schedule || []).join("\n"),
  };
}

/** @param {any} draft */
export function picturePayload(draft) {
  const { schedule_text, ...config } = draft;
  return {
    ...config, schedule: (schedule_text || "").split("\n").map((spec) => spec.trim()).filter(Boolean), name: draft.name.trim(), title: draft.title.trim(),
    namespace: draft.namespace.trim().toLowerCase() || "default",
    description: draft.description.trim(), prompt: draft.prompt.trim(),
    sources: draft.sources.map(({ kind, ref }) => ({ kind, ref })),
  };
}

/** @param {any[]} documents */
export function pictureDocumentTree(documents) {
  const children = { "": [] };
  const directories = new Set();
  for (const doc of documents) {
    const segments = doc.path.split("/");
    let parent = "";
    for (let i = 0; i < segments.length - 1; i++) {
      const folder = segments.slice(0, i + 1).join("/");
      if (!directories.has(folder)) {
        directories.add(folder);
        children[parent].push({ path: folder, is_dir: true });
        children[folder] = [];
      }
      parent = folder;
    }
    children[parent].push({ path: doc.path, is_dir: false, title: doc.title });
  }
  for (const entries of Object.values(children)) entries.sort((a, b) => Number(b.is_dir) - Number(a.is_dir) || a.path.localeCompare(b.path));
  return { entries: children[""], children };
}

export function pictureURL(name, revision = "", doc = "", anchor = "") {
  const query = new URLSearchParams();
  if (revision) query.set("revision", revision);
  if (doc) query.set("doc", doc);
  if (anchor) query.set("anchor", anchor);
  const suffix = query.toString();
  return `/big-pictures/${encodeURIComponent(name)}${suffix ? `?${suffix}` : ""}`;
}

// Return undefined for ordinary external links, null for invalid document
// references, or a hash-router URL for a document in this exact publication.
export function pictureDocumentLink(href, name, revision, currentPath, documents) {
  if (/^(?:[a-z][a-z0-9+.-]*:|\/\/|\/)/i.test(href)) return undefined;
  const hash = href.indexOf("#");
  let target = hash < 0 ? href : href.slice(0, hash);
  let anchor = hash < 0 ? "" : href.slice(hash + 1);
  try { target = decodeURIComponent(target); anchor = decodeURIComponent(anchor); }
  catch { return null; }
  if (target && !target.endsWith(".md")) return undefined;
  const segments = target ? currentPath.split("/").slice(0, -1) : [];
  if (target) {
    for (const part of target.split("/")) {
      if (part === "..") { if (!segments.length) return null; segments.pop(); }
      else if (part && part !== ".") segments.push(part);
    }
  }
  const path = target ? segments.join("/") : currentPath;
  if (!documents.some((doc) => doc.path === path)) return null;
  return `#${pictureURL(name, revision, path, anchor)}`;
}
