export const API_METHOD_COLORS = {
  GET: "text-ok",
  POST: "text-busy",
  PUT: "text-warn",
  PATCH: "text-warn",
  DELETE: "text-err",
  GRPC: "text-busy",
};

export function createOperationRequest(detail) {
  const parameters = detail.parameters || [];
  const prefill = (parameter) => parameter.example ?? parameter.default ?? "";
  return {
    pathParams: parameters
      .filter((parameter) => parameter.in === "path")
      .map((parameter) => ({ name: parameter.name, value: prefill(parameter), required: true })),
    query: parameters
      .filter((parameter) => parameter.in === "query")
      .map((parameter) => ({ name: parameter.name, value: prefill(parameter), required: !!parameter.required })),
    headers: [{ name: "", value: "" }],
    body: detail.request?.body != null ? JSON.stringify(detail.request.body, null, 2) : "",
  };
}

export function operationRowsToMap(rows, { keepEmpty = false } = {}) {
  const values = {};
  for (const row of rows) {
    const name = (row.name || "").trim();
    if (!name) continue;
    if (!keepEmpty && row.value === "") continue;
    values[name] = row.value;
  }
  return values;
}

export function buildOperationCallPayload(operationId, request) {
  const body = request.body.trim();
  return {
    endpoint: operationId,
    path_params: operationRowsToMap(request.pathParams),
    query: operationRowsToMap(request.query),
    headers: operationRowsToMap(request.headers, { keepEmpty: true }),
    ...(body ? { body: JSON.parse(body) } : {}),
  };
}

export function prettyOperationBody(response) {
  if (!response?.body) return "";
  if ((response.content_type || "").includes("json")) {
    try {
      return JSON.stringify(JSON.parse(response.body), null, 2);
    } catch {
      // A mislabeled response is still useful as raw text.
    }
  }
  return response.body;
}
