import assert from "node:assert/strict";
import test from "node:test";
import { createPictureDraft, picturePayload, pictureDocumentTree, pictureURL, pictureDocumentLink } from "./big-picture.js";

test("workspace drafts own source selections and preserve optimistic version", () => {
  const saved = { name: "commerce", version: 7, sources: [{ kind: "repo", ref: "example/team/repo" }] };
  const draft = createPictureDraft(saved);
  draft.sources[0].ref = "edited";
  draft.namespace = " Commerce ";
  assert.equal(saved.sources[0].ref, "example/team/repo");
  assert.equal(picturePayload(draft).expected_version, 7);
  assert.equal(picturePayload(draft).namespace, "commerce");
});

test("schedule drafts round-trip lines without leaking UI-only fields", () => {
  const draft = createPictureDraft({ schedule: ["@every 6h", "0 2 * * *"] });
  assert.equal(draft.schedule_text, "@every 6h\n0 2 * * *");
  const payload = picturePayload(draft);
  assert.deepEqual(payload.schedule, ["@every 6h", "0 2 * * *"]);
  assert.equal("schedule_text" in payload, false);
  draft.schedule_text = "";
  assert.deepEqual(picturePayload(draft).schedule, []);
});

const docs = [{ path: "overview.md", title: "Overview" }, { path: "services/checkout.md", title: "Checkout" }, { path: "services/payments/retries.md", title: "Retries" }];

test("document tree creates each shared folder once, with directories first", () => {
  const tree = pictureDocumentTree(docs);
  assert.deepEqual(tree.entries.map((entry) => entry.path), ["services", "overview.md"]);
  assert.deepEqual(tree.children.services.map((entry) => entry.path), ["services/payments", "services/checkout.md"]);
  assert.equal(tree.children["services/payments"][0].path, "services/payments/retries.md");
});

test("document links pin revision and resolve relative references", () => {
  assert.equal(pictureDocumentLink("../overview.md#flow", "commerce", "rev", "services/checkout.md", docs), `#${pictureURL("commerce", "rev", "overview.md", "flow")}`);
  assert.equal(pictureDocumentLink("#details", "commerce", "rev", "services/checkout.md", docs), `#${pictureURL("commerce", "rev", "services/checkout.md", "details")}`);
  assert.equal(pictureDocumentLink("./payments/retries.md", "commerce", "rev", "services/checkout.md", docs), `#${pictureURL("commerce", "rev", "services/payments/retries.md")}`);
});

test("missing/traversing/malformed document links are disabled, external links unchanged", () => {
  for (const href of ["../../escape.md", "../missing.md", "%ZZ.md"]) assert.equal(pictureDocumentLink(href, "commerce", "rev", "services/checkout.md", docs), null);
  for (const href of ["https://example.com/readme.md", "/api/v1/health", "mailto:team@example.com"]) assert.equal(pictureDocumentLink(href, "commerce", "rev", "overview.md", docs), undefined);
});
