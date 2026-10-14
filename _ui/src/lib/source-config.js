// Schema-driven config forms for web-source types that publish field
// descriptors (GET /sources/types). The hand-written providers (pages,
// confluence, jira) keep their dedicated forms in Sources.svelte.

export const builtinFormTypes = new Set(["pages", "confluence", "jira"]);

// formValues builds the editable form state from descriptor fields and, when
// editing, the redacted config view the server returned.
export function formValues(fields = [], view = null) {
  const values = {};
  for (const f of fields) {
    const current = view ? view[f.key] : undefined;
    switch (f.kind) {
      case "bool":
        values[f.key] = current ?? f.default ?? false;
        break;
      case "list":
        values[f.key] = (current || []).join(", ");
        break;
      case "number":
        values[f.key] = current ?? "";
        break;
      case "secret":
        values[f.key] = "";
        break;
      case "map":
        // Header-style maps: names are shown, values are write-only.
        values[f.key] = (view?.[`${f.key}_names`] || []).map((name) => `${name}=`).join("\n");
        break;
      default:
        values[f.key] = current ?? (f.default !== undefined && !view ? String(f.default) : "");
    }
  }
  return values;
}

// secretIsSet reports whether the server holds a value for a secret field.
export function secretIsSet(view, key) {
  return !!view?.[`${key}_set`];
}

function parseMap(text) {
  const out = {};
  for (const line of String(text || "").split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const i = trimmed.indexOf("=");
    const k = (i < 0 ? trimmed : trimmed.slice(0, i)).trim();
    if (k) out[k] = i < 0 ? "" : trimmed.slice(i + 1).trim();
  }
  return out;
}

// formConfig turns form state into the provider config payload. Blank
// optional values are omitted on create and sent as null on edit, so clearing
// a field clears it while untouched secrets (blank) keep their stored value.
export function formConfig(fields = [], values = {}, editing = false) {
  const out = {};
  for (const f of fields) {
    const v = values[f.key];
    switch (f.kind) {
      case "bool":
        out[f.key] = !!v;
        break;
      case "number": {
        if (v === "" || v === null || v === undefined) {
          if (editing) out[f.key] = null;
          break;
        }
        const n = Number(v);
        if (!Number.isNaN(n)) out[f.key] = n;
        break;
      }
      case "list": {
        const list = String(v || "")
          .split(",")
          .map((x) => x.trim())
          .filter(Boolean);
        if (list.length || editing) out[f.key] = list;
        break;
      }
      case "secret":
        if (v) out[f.key] = v;
        break;
      case "map": {
        const m = parseMap(v);
        if (Object.keys(m).length || editing) out[f.key] = m;
        break;
      }
      default: {
        const s = String(v ?? "").trim();
        if (s) out[f.key] = f.kind === "text" ? String(v) : s;
        else if (editing) out[f.key] = null;
      }
    }
  }
  return out;
}

// fileToPushItem reads a browser File into a push item, base64-encoding
// binary formats.
export async function fileToPushItem(file, id = "") {
  const textual = /^(text\/|application\/(json|x-yaml|yaml|xml))/.test(file.type) ||
    /\.(md|markdown|txt|rst|adoc|csv|tsv|json|ya?ml|html?)$/i.test(file.name);
  const item = { id: id || file.name, title: file.name.replace(/\.[^.]+$/, ""), content_type: file.type || "" };
  if (textual) {
    item.content = await file.text();
    if (!item.content_type) item.content_type = /\.html?$/i.test(file.name) ? "text/html" : "text/markdown";
    return item;
  }
  const bytes = new Uint8Array(await file.arrayBuffer());
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  item.content = btoa(binary);
  item.encoding = "base64";
  if (!item.content_type) item.content_type = "application/octet-stream";
  return item;
}
