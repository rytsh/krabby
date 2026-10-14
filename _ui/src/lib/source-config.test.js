import { test } from "node:test";
import assert from "node:assert/strict";

import { fileToPushItem, formConfig, formValues, secretIsSet } from "./source-config.js";

const fields = [
  { key: "url", kind: "string" },
  { key: "token", kind: "secret" },
  { key: "page_size", kind: "number", default: 100 },
  { key: "team_paths", kind: "list" },
  { key: "headers", kind: "map" },
  { key: "ignore_robots", kind: "bool" },
  { key: "template", kind: "text" },
];

test("formValues fills defaults for a new source", () => {
  const v = formValues(fields);
  assert.equal(v.url, "");
  assert.equal(v.page_size, "");
  assert.equal(v.ignore_robots, false);
});

test("formValues restores a redacted view without secrets", () => {
  const view = { url: "https://x", token_set: true, team_paths: ["a", "b"], headers_names: ["X-Key"], page_size: 50 };
  const v = formValues(fields, view);
  assert.equal(v.token, "");
  assert.equal(v.team_paths, "a, b");
  assert.equal(v.headers, "X-Key=");
  assert.equal(v.page_size, 50);
  assert.ok(secretIsSet(view, "token"));
});

test("formConfig omits blanks on create and clears them on edit", () => {
  const values = { url: " https://x ", token: "", page_size: "", team_paths: "a, ,b", headers: "X-Key=1\n", ignore_robots: true, template: "" };
  assert.deepEqual(formConfig(fields, values), {
    url: "https://x", team_paths: ["a", "b"], headers: { "X-Key": "1" }, ignore_robots: true,
  });
  const edited = formConfig(fields, { ...values, url: "", headers: "X-Key=" }, true);
  assert.equal(edited.url, null);
  assert.equal(edited.page_size, null);
  assert.equal("token" in edited, false);
  assert.deepEqual(edited.headers, { "X-Key": "" });
});

test("fileToPushItem base64-encodes binary files", async () => {
  const pdf = new File([new Uint8Array([0x25, 0x50, 0x44, 0x46])], "spec.pdf", { type: "application/pdf" });
  const item = await fileToPushItem(pdf);
  assert.equal(item.encoding, "base64");
  assert.equal(item.content, "JVBERg==");
  assert.equal(item.id, "spec.pdf");

  const md = await fileToPushItem(new File(["# hi"], "notes.md"));
  assert.equal(md.content, "# hi");
  assert.equal(md.content_type, "text/markdown");
});
