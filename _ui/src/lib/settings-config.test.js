import assert from "node:assert/strict";
import test from "node:test";

import {
  buildDocsPayload,
  buildLangfusePayload,
  buildRuntimePayload,
  createDocsDraft,
  createLangfuseDraft,
  createRuntimeDraft,
  normalizeSettingsSnapshot,
} from "./settings-config.js";

const serverConfig = {
  task_concurrency: 3,
  repo_schedules: [{ namespace: " team ", specs: [" 0 * * * * ", ""], disabled: false }],
  webhook_secret_set: true,
  docs_enabled: true,
  docs_prompt: "server prompt",
  docs_default_prompt: "default prompt",
  docs_include: ["**/*.go"],
  rag_lexical_stop_words: ["the"],
  llm_model: "server-model",
  llm_api_key_set: true,
  embed_api_key_set: true,
  code_embed_api_key_set: false,
  langfuse_enabled: true,
  langfuse_host: "https://langfuse.example",
  langfuse_capture: "full",
  langfuse_secret_key_set: true,
  updated_at: "2026-08-29T00:00:00Z",
  server_only_field: "must not leak",
};

test("normalized snapshots are immutable and drafts do not share mutable state", () => {
  const snapshot = normalizeSettingsSnapshot(serverConfig);
  const runtime = createRuntimeDraft(snapshot);
  const docs = createDocsDraft(snapshot);
  const langfuse = createLangfuseDraft(snapshot);

  assert.ok(Object.isFrozen(snapshot));
  assert.ok(Object.isFrozen(snapshot.repo_schedules));
  assert.ok(Object.isFrozen(snapshot.repo_schedules[0].specs));

  runtime.repo_schedules[0].namespace = "changed";
  docs.docs_include.push("**/*.js");
  langfuse.langfuse_host = "https://draft.example";

  assert.equal(snapshot.repo_schedules[0].namespace, " team ");
  assert.deepEqual(snapshot.docs_include, ["**/*.go"]);
  assert.equal(snapshot.langfuse_host, "https://langfuse.example");
});

test("runtime payload contains only runtime fields and its current secret", () => {
  const draft = createRuntimeDraft(normalizeSettingsSnapshot(serverConfig));
  draft.docs_prompt = "unsaved docs edit";
  draft.langfuse_host = "https://unsaved.example";

  assert.deepEqual(buildRuntimePayload(draft, "new webhook"), {
    task_concurrency: 3,
    repo_schedules: [{ namespace: "team", specs: ["0 * * * *"], disabled: false }],
    webhook_secret: "new webhook",
  });
  assert.deepEqual(buildRuntimePayload(draft, "", true).webhook_secret, "");
});

test("docs payload uses an explicit docs allowlist and current draft values", () => {
  const draft = createDocsDraft(normalizeSettingsSnapshot(serverConfig));
  draft.llm_model = "unsaved-model";
  draft.rag_lexical_stop_words_text = " the, a, , an ";
  draft.task_concurrency = 99;
  draft.langfuse_host = "https://unsaved.example";
  draft.server_only_field = "leak";

  const payload = buildDocsPayload(draft, { llm: "llm-secret", embed: "embed-secret", codeEmbed: "code-secret" });
  assert.equal(payload.llm_model, "unsaved-model");
  assert.deepEqual(payload.rag_lexical_stop_words, ["the", "a", "an"]);
  assert.equal(payload.llm_api_key, "llm-secret");
  assert.equal(payload.embed_api_key, "embed-secret");
  assert.equal(payload.code_embed_api_key, "code-secret");
  assert.ok(!Object.hasOwn(payload, "task_concurrency"));
  assert.ok(!Object.hasOwn(payload, "langfuse_host"));
  assert.ok(!Object.hasOwn(payload, "server_only_field"));
  assert.ok(!Object.hasOwn(payload, "updated_at"));
  assert.ok(!Object.hasOwn(payload, "docs_default_prompt"));
});

test("Langfuse payload cannot submit runtime or Docs/RAG drafts", () => {
  const draft = createLangfuseDraft(normalizeSettingsSnapshot(serverConfig));
  draft.langfuse_host = "https://draft.example";
  draft.docs_prompt = "unsaved docs edit";
  draft.repo_schedules = [{ namespace: "other", specs: ["@every 1m"] }];

  const payload = buildLangfusePayload(draft, "langfuse-secret");
  assert.equal(payload.langfuse_host, "https://draft.example");
  assert.equal(payload.langfuse_secret_key, "langfuse-secret");
  assert.ok(!Object.hasOwn(payload, "docs_prompt"));
  assert.ok(!Object.hasOwn(payload, "repo_schedules"));
  assert.ok(!Object.hasOwn(payload, "langfuse_secret_key_set"));
});
