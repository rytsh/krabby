import assert from "node:assert/strict";
import test from "node:test";
import { createExternalMCPDraft, buildExternalMCPPayload, identifierList } from "./external-mcp-config.js";

const connection = {
  name: "config", url: "https://example.com/mcp", bearer_token_set: true,
  header_names: ["X-Api-Key"], allowed_tools: ["get_config"], allowed_resources: ["config://production"],
};

test("drafts contain no saved secrets and own their grants", () => {
  const draft = createExternalMCPDraft(connection);
  assert.equal(draft.bearer_token, "");
  assert.deepEqual(draft.headers, [{ name: "X-Api-Key", value: "" }]);
  draft.allowed_tools.push("other");
  assert.deepEqual(connection.allowed_tools, ["get_config"]);
  assert.equal(createExternalMCPDraft().timeout_seconds, 30);
});

test("unchanged endpoint preserves headers using blank placeholders", () => {
  const payload = buildExternalMCPPayload(createExternalMCPDraft(connection), connection);
  assert.deepEqual(payload.headers, { "X-Api-Key": "" });
  assert.equal(payload.bearer_token, "");
  assert.deepEqual(payload.allowed_tools, ["get_config"]);
});

test("changed endpoint drops old grants and blank saved header placeholders", () => {
  const draft = createExternalMCPDraft(connection);
  draft.url = "https://new.example.com/mcp";
  draft.headers.push({ name: "X-New-Key", value: "new-secret" });
  const payload = buildExternalMCPPayload(draft, connection);
  assert.deepEqual(payload.headers, { "X-New-Key": "new-secret" });
  assert.deepEqual(payload.allowed_tools, []);
  assert.deepEqual(payload.allowed_resources, []);
});

test("header deletion, duplicate detection and exact identifier lists", () => {
  const draft = createExternalMCPDraft(connection);
  draft.headers = [];
  assert.deepEqual(buildExternalMCPPayload(draft, connection).headers, {});
  draft.headers = [{ name: "X-Key", value: "a" }, { name: "x-key", value: "b" }];
  assert.throws(() => buildExternalMCPPayload(draft), /unique/);
  assert.deepEqual(identifierList(" config://prod\nget_config\nget_config\n \n"), ["config://prod", "get_config"]);
});
