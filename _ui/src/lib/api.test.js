import assert from "node:assert/strict";
import test from "node:test";

import { api } from "./api.js";

test("importSourcePages forwards AbortSignal to fetch", async () => {
  const originalFetch = globalThis.fetch;
  const controller = new AbortController();
  let fetchOptions;
  globalThis.fetch = async (url, options) => {
    assert.equal(url, "api/v1/sources/docs/pages/import");
    fetchOptions = options;
    return new Response('{"imported":1}', {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  try {
    assert.deepEqual(
      await api.importSourcePages("docs", [{ url: "https://docs.example/page" }], {
        signal: controller.signal,
      }),
      { imported: 1 },
    );
  } finally {
    globalThis.fetch = originalFetch;
  }

  assert.equal(fetchOptions.method, "POST");
  assert.equal(fetchOptions.signal, controller.signal);
});
