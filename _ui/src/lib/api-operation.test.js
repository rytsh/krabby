import assert from "node:assert/strict";
import test from "node:test";

import {
  buildOperationCallPayload,
  createOperationRequest,
  operationRowsToMap,
  prettyOperationBody,
} from "./api-operation.js";

test("operation request drafts are initialized from declared examples", () => {
  const request = createOperationRequest({
    parameters: [
      { in: "path", name: "id", example: "inv-1" },
      { in: "query", name: "limit", default: 25, required: true },
    ],
    request: { body: { paid: true } },
  });

  assert.deepEqual(request.pathParams, [{ name: "id", value: "inv-1", required: true }]);
  assert.deepEqual(request.query, [{ name: "limit", value: 25, required: true }]);
  assert.equal(request.body, '{\n  "paid": true\n}');
});

test("operation request drafts preserve zero and false examples", () => {
  const request = createOperationRequest({
    parameters: [
      { in: "path", name: "index", example: 0, default: 7 },
      { in: "query", name: "enabled", example: false, default: true },
    ],
    request: { body: false },
  });

  assert.equal(request.pathParams[0].value, 0);
  assert.equal(request.query[0].value, false);
  assert.equal(request.body, "false");
});

test("operation payload drops blank parameters but keeps empty header overrides", () => {
  const payload = buildOperationCallPayload("getInvoice", {
    pathParams: [{ name: " id ", value: "inv-1" }],
    query: [{ name: "filter", value: "" }],
    headers: [
      { name: " Authorization ", value: "" },
      { name: "", value: "ignored" },
    ],
    body: '{"expand":true}',
  });

  assert.deepEqual(payload, {
    endpoint: "getInvoice",
    path_params: { id: "inv-1" },
    query: {},
    headers: { Authorization: "" },
    body: { expand: true },
  });
  assert.deepEqual(operationRowsToMap([{ name: "x", value: "" }]), {});
});

test("operation response bodies pretty-print valid JSON and preserve other text", () => {
  assert.equal(prettyOperationBody({ body: '{"ok":true}', content_type: "application/json" }), '{\n  "ok": true\n}');
  assert.equal(prettyOperationBody({ body: "not-json", content_type: "application/json" }), "not-json");
  assert.equal(prettyOperationBody({ body: "plain", content_type: "text/plain" }), "plain");
});
