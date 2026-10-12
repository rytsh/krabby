// Memory view helpers kept out of the component so they can be unit-tested.

export const MEMORY_KINDS = ["decision", "convention", "gotcha", "fact", "preference", "todo", "note"];
export const MEMORY_STATUSES = ["active", "stale", "orphaned", "expired"];

/** @param {string} id */
export function memoryURL(id = "") {
  return id ? `/memories/${encodeURIComponent(id)}` : "/memories";
}

/** @param {string} status */
export function statusTone(status) {
  if (status === "active") return "ok";
  if (status === "stale") return "warn";
  if (status === "orphaned") return "err";
  return "faint";
}

/**
 * Groups spaces for the sidebar: the shared default first, then team spaces,
 * then agent spaces ("agent:<name>").
 * @param {{ space: string, count: number }[]} spaces
 */
export function groupSpaces(spaces) {
  const shared = [], teams = [], agents = [];
  for (const s of spaces) {
    if (s.space === "default") shared.push(s);
    else if (s.space.startsWith("agent:")) agents.push(s);
    else teams.push(s);
  }
  return [
    { label: "Shared", items: shared.length ? shared : [{ space: "default", count: 0 }] },
    { label: "Teams", items: teams },
    { label: "Agents", items: agents },
  ].filter((g) => g.items.length > 0);
}

/** @param {any} memory */
export function createMemoryDraft(memory = null, author = "") {
  return {
    author,
    space: memory?.space || "default",
    kind: memory?.kind || "note",
    title: memory?.title || "",
    content: memory?.content || "",
    tags_text: (memory?.tags || []).join(", "),
    evidence_text: (memory?.evidence || []).map(formatEvidence).join("\n"),
    expires_at: memory?.expires_at ? memory.expires_at.slice(0, 10) : "",
  };
}

/** @param {any} e */
export function formatEvidence(e) {
  let out = `${e.repo}:${e.path || ""}`;
  if (e.start_line) out += `#L${e.start_line}${e.end_line && e.end_line !== e.start_line ? `-${e.end_line}` : ""}`;
  if (e.symbol) out += ` @${e.symbol}`;
  return out;
}

/**
 * Parses one evidence line: "repo:path#L10-20 @Symbol", "repo:path",
 * or "repo:@Symbol".
 * @param {string} line
 */
export function parseEvidence(line) {
  const text = line.trim();
  if (!text) return null;
  let rest = text, symbol = "";
  const at = rest.lastIndexOf(" @");
  if (at >= 0) { symbol = rest.slice(at + 2).trim(); rest = rest.slice(0, at).trim(); }
  else if (rest.includes(":@")) { symbol = rest.slice(rest.indexOf(":@") + 2).trim(); rest = rest.slice(0, rest.indexOf(":@") + 1); }
  const colon = rest.lastIndexOf(":");
  if (colon <= 0) throw new Error(`Evidence "${text}" must look like repo:path#L10-20`);
  const repo = rest.slice(0, colon).trim();
  let path = rest.slice(colon + 1).trim();
  let start_line = 0, end_line = 0;
  const hash = path.match(/#L(\d+)(?:-L?(\d+))?$/);
  if (hash) {
    path = path.slice(0, hash.index);
    start_line = Number(hash[1]);
    end_line = Number(hash[2] || hash[1]);
  }
  if (!repo || (!path && !symbol)) throw new Error(`Evidence "${text}" needs a repository and a path or symbol`);
  return { repo, path, symbol, start_line, end_line };
}

// The server stores no expiry as the zero time; sending it clears one.
const NO_EXPIRY = "0001-01-01T00:00:00Z";

/** @param {ReturnType<typeof createMemoryDraft>} draft */
export function memoryPayload(draft) {
  const payload = {
    author: (draft.author || "").trim(),
    space: draft.space.trim().toLowerCase() || "default",
    kind: draft.kind,
    title: draft.title.trim(),
    content: draft.content.trim(),
    tags: draft.tags_text.split(",").map((t) => t.trim()).filter(Boolean),
    evidence: draft.evidence_text.split("\n").map(parseEvidence).filter(Boolean),
  };
  payload.expires_at = draft.expires_at ? new Date(`${draft.expires_at}T23:59:59Z`).toISOString() : NO_EXPIRY;
  return payload;
}
