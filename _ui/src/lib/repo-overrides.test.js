import assert from "node:assert/strict";
import test from "node:test";

import {
  buildRepoOverridesPayload,
  createRepoOverridesDraft,
  hasRepoOverrides,
  repoSettingsRows,
} from "./repo-overrides.js";

test("repo override drafts normalize API values without sharing arrays", () => {
  const repo = {
    overrides: {
      include: ["**/*.go"],
      docs_max_source_bytes: 49152,
      skip_stages: ["docs"],
    },
  };
  const draft = createRepoOverridesDraft(repo);

  assert.equal(draft.include, "**/*.go");
  assert.equal(draft.docs_max_source_bytes, "49152");
  draft.skip_stages.push("graph");
  assert.deepEqual(repo.overrides.skip_stages, ["docs"]);
});

test("repo override payload trims lists, text, and positive byte limits", () => {
  const draft = createRepoOverridesDraft();
  Object.assign(draft, {
    include: " **/*.go, , **/*.js ",
    graph_exclude: " generated/ ",
    docs_prompt: " custom prompt ",
    docs_max_source_bytes: "2048",
    docs_max_group_bytes: "-1",
    docs_max_synthesis_bytes: "not a number",
    skip_stages: ["graph"],
  });

  assert.deepEqual(buildRepoOverridesPayload(draft), {
    include: ["**/*.go", "**/*.js"],
    include_extra: [],
    exclude: [],
    graph_exclude: ["generated/"],
    docs_prompt: "custom prompt",
    docs_prompt_extra: "",
    docs_max_source_bytes: 2048,
    docs_max_group_bytes: 0,
    docs_max_synthesis_bytes: 0,
    skip_stages: ["graph"],
  });
});

test("repo override summary marks effective repo values", () => {
  const rows = repoSettingsRows({
    effective: {
      code_include_is_default: true,
      docs_limits: { max_source_bytes: 49152 },
      docs_prompt_source: "repo",
      docs_prompt_extras: ["global", "repo"],
    },
    overrides: { docs_max_source_bytes: 49152 },
  });

  assert.deepEqual(
    rows.map(({ label, value, overridden }) => [label, value, overridden]),
    [
      ["Indexed files", "built-in allowlist", false],
      ["Docs input budget", "48 KiB per file", true],
      ["Docs prompt", "repo", true],
      ["Extra doc rules", "global + repo", true],
    ],
  );
  assert.equal(hasRepoOverrides({ overrides: { docs_prompt: "custom" } }), true);
  assert.equal(hasRepoOverrides({ overrides: { graph_exclude: ["vendor/**"] } }), true);
  assert.equal(hasRepoOverrides({ overrides: {} }), false);
});
