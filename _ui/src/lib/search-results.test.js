import assert from "node:assert/strict";
import test from "node:test";

import { docsSearchPage } from "./search-results.js";

test("docs search preserves results and the actual mode from the backend envelope", () => {
  const results = [{ repo: "web:jira", path: "issue-1.md", excerpt: "Ticket" }];
  assert.deepEqual(docsSearchPage({ results, mode: "lexical" }, "semantic"), {
    results,
    mode: "lexical",
    note: "",
  });
});

test("empty docs search preserves the backend explanation", () => {
  const note = "The collection has not synced yet.";
  assert.deepEqual(docsSearchPage({ results: [], mode: "semantic", note }, "semantic"), {
    results: [],
    mode: "semantic",
    note,
  });
});

test("docs search supports legacy bare arrays", () => {
  const results = [{ repo: "web:confluence", path: "page-1.md" }];
  assert.deepEqual(docsSearchPage(results, "hybrid"), { results, mode: "hybrid", note: "" });
});
