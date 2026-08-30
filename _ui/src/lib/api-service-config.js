export function createApiServiceForm() {
  return {
    name: "",
    kind: "openapi",
    group: "",
    description: "",
    base_url: "",
    refresh_interval: "24h",
    schedule: "",
    spec_patch: "",
    operations: "",
    url: "",
    user: "",
    token: "",
    insecure_skip_verify: false,
    target: "",
    plaintext: false,
    server_name: "",
    grpc_services: "",
  };
}

export function apiServiceFormFromService(service) {
  const config = service.config || {};
  return {
    ...createApiServiceForm(),
    name: service.name,
    kind: service.kind,
    group: service.effective_group === "default" ? "" : service.effective_group,
    description: service.description || "",
    base_url: service.base_url || "",
    refresh_interval: service.refresh_interval || "manual",
    schedule: (service.specs || []).join(", "),
    spec_patch: service.spec_patch ? JSON.stringify(service.spec_patch, null, 2) : "",
    operations: service.operations ? JSON.stringify(service.operations, null, 2) : "",
    url: config.url || "",
    user: config.user || "",
    token: "",
    insecure_skip_verify: !!config.insecure_skip_verify,
    target: config.target || "",
    plaintext: !!config.plaintext,
    server_name: config.server_name || "",
    grpc_services: (config.services || []).join(", "),
  };
}

export function parseApiServiceJSON(raw, label) {
  const text = raw.trim();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch (error) {
    throw new Error(`${label}: ${error.message}`, { cause: error });
  }
}

export function buildApiProviderConfig(form) {
  if (form.kind === "grpc") {
    const config = {
      target: form.target.trim(),
      plaintext: form.plaintext,
      insecure_skip_verify: form.insecure_skip_verify,
      token: form.token,
    };
    if (form.server_name.trim()) config.server_name = form.server_name.trim();
    const services = form.grpc_services
      .split(",")
      .map((service) => service.trim())
      .filter(Boolean);
    config.services = services.length ? services : null;
    return config;
  }
  return {
    url: form.url.trim(),
    user: form.user.trim(),
    token: form.token,
    insecure_skip_verify: form.insecure_skip_verify,
  };
}

export function buildApiServicePayload(form) {
  const specs = form.schedule
    .split(",")
    .map((spec) => spec.trim())
    .filter(Boolean);

  return {
    name: form.name.trim().toLowerCase(),
    kind: form.kind,
    group: form.group.trim(),
    description: form.description.trim(),
    base_url: form.base_url.trim(),
    refresh_interval: form.refresh_interval === "manual" ? "" : form.refresh_interval,
    specs,
    spec_patch: parseApiServiceJSON(form.spec_patch, "Spec patch"),
    operations: parseApiServiceJSON(form.operations, "Operation overrides"),
    config: buildApiProviderConfig(form),
  };
}

export function buildApiConfigTestPayload(form, existingName = "") {
  return {
    kind: form.kind,
    existing_name: existingName,
    config: buildApiProviderConfig(form),
    spec_patch: parseApiServiceJSON(form.spec_patch, "Spec patch"),
  };
}
