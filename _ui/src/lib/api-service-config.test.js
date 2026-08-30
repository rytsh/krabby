import assert from "node:assert/strict";
import test from "node:test";

import {
  apiServiceFormFromService,
  buildApiConfigTestPayload,
  buildApiServicePayload,
  createApiServiceForm,
  parseApiServiceJSON,
} from "./api-service-config.js";

test("API service payload normalizes OpenAPI form values", () => {
  const form = createApiServiceForm();
  Object.assign(form, {
    name: " Billing ",
    group: " finance ",
    description: " invoices ",
    base_url: " https://billing.internal ",
    refresh_interval: "manual",
    schedule: " 0 3 * * *, , @every 6h ",
    spec_patch: '{"info":{"title":"Internal"}}',
    operations: '{"deleteAll":{"hidden":true}}',
    url: " https://docs.example/openapi.json ",
    user: " api-user ",
    token: "secret",
  });

  assert.deepEqual(buildApiServicePayload(form), {
    name: "billing",
    kind: "openapi",
    group: "finance",
    description: "invoices",
    base_url: "https://billing.internal",
    refresh_interval: "",
    specs: ["0 3 * * *", "@every 6h"],
    spec_patch: { info: { title: "Internal" } },
    operations: { deleteAll: { hidden: true } },
    config: {
      url: "https://docs.example/openapi.json",
      user: "api-user",
      token: "secret",
      insecure_skip_verify: false,
    },
  });
});

test("gRPC provider payload keeps optional service semantics", () => {
  const form = createApiServiceForm();
  Object.assign(form, {
    kind: "grpc",
    target: " billing.internal:443 ",
    plaintext: true,
    server_name: " billing.internal ",
    grpc_services: " billing.v1.Invoices, , billing.v1.Payments ",
  });

  assert.deepEqual(buildApiConfigTestPayload(form, "billing"), {
    kind: "grpc",
    existing_name: "billing",
    config: {
      target: "billing.internal:443",
      plaintext: true,
      insecure_skip_verify: false,
      token: "",
      server_name: "billing.internal",
      services: ["billing.v1.Invoices", "billing.v1.Payments"],
    },
    spec_patch: null,
  });
});

test("service records become editable forms without exposing stored tokens", () => {
  const form = apiServiceFormFromService({
    name: "billing",
    kind: "grpc",
    effective_group: "default",
    refresh_interval: "",
    specs: ["@every 1h"],
    config: { target: "billing:443", token: "redacted", services: ["billing.v1.Invoices"] },
  });

  assert.equal(form.group, "");
  assert.equal(form.refresh_interval, "manual");
  assert.equal(form.schedule, "@every 1h");
  assert.equal(form.token, "");
  assert.equal(form.grpc_services, "billing.v1.Invoices");
});

test("invalid service JSON identifies the field", () => {
  assert.throws(() => parseApiServiceJSON("{", "Spec patch"), (error) => {
    assert.match(error.message, /^Spec patch:/);
    assert.ok(error.cause instanceof SyntaxError);
    return true;
  });
});
