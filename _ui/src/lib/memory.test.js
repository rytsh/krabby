import { test } from "node:test";
import assert from "node:assert/strict";
import { parseEvidence, formatEvidence, memoryPayload, createMemoryDraft, groupSpaces } from "./memory.js";

test("parseEvidence handles paths, ranges and symbols", () => {
  assert.deepEqual(parseEvidence("example.com/a/svc:queue/retry.go#L10-20"), { repo: "example.com/a/svc", path: "queue/retry.go", symbol: "", start_line: 10, end_line: 20 });
  assert.deepEqual(parseEvidence("svc:main.go#L5 @Run"), { repo: "svc", path: "main.go", symbol: "Run", start_line: 5, end_line: 5 });
  assert.deepEqual(parseEvidence("svc:@Retry"), { repo: "svc", path: "", symbol: "Retry", start_line: 0, end_line: 0 });
  assert.equal(parseEvidence("  "), null);
  assert.throws(() => parseEvidence("no-colon"));
});

test("evidence round-trips through the draft", () => {
  const e = { repo: "svc", path: "a.go", symbol: "A", start_line: 3, end_line: 9 };
  assert.deepEqual(parseEvidence(formatEvidence(e)), e);
});

test("memoryPayload normalizes the draft", () => {
  const draft = { ...createMemoryDraft(), space: " Team ", title: " T ", content: " body ", tags_text: "a, ,b", evidence_text: "svc:a.go\n" };
  const p = memoryPayload(draft);
  assert.equal(p.space, "team");
  assert.equal(p.author, "");
  assert.equal(memoryPayload({ ...draft, author: " Eray " }).author, "Eray");
  assert.deepEqual(p.tags, ["a", "b"]);
  assert.equal(p.evidence.length, 1);
  assert.equal(p.expires_at, "0001-01-01T00:00:00Z");
  assert.equal(memoryPayload({ ...draft, expires_at: "2026-12-31" }).expires_at, "2026-12-31T23:59:59.000Z");
});

test("groupSpaces orders shared, teams, agents", () => {
  const groups = groupSpaces([{ space: "agent:bot", count: 1 }, { space: "payments", count: 2 }]);
  assert.deepEqual(groups.map((g) => g.label), ["Shared", "Teams", "Agents"]);
  assert.equal(groups[0].items[0].space, "default");
});
