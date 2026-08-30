const RUNTIME_FIELDS = ["task_concurrency", "repo_schedules"];

const DOCS_FIELDS = [
  "docs_enabled",
  "docs_concurrency",
  "docs_summary_model",
  "docs_max_groups",
  "docs_include",
  "docs_include_extra",
  "docs_exclude",
  "docs_prompt",
  "docs_prompt_extra",
  "docs_max_source_bytes",
  "docs_max_group_bytes",
  "docs_max_synthesis_bytes",
  "llm_base_url",
  "llm_model",
  "llm_timeout",
  "web_image_analysis_enabled",
  "web_image_model",
  "web_image_max_per_page",
  "web_image_max_bytes",
  "web_image_max_pixels",
  "web_image_allow_authenticated",
  "embed_base_url",
  "embed_model",
  "embed_dim",
  "embed_batch",
  "embed_concurrency",
  "embed_timeout",
  "rag_enabled",
  "rag_keep_markdown_targets",
  "rag_chunk_size",
  "rag_chunk_overlap",
  "rag_top_k",
  "rag_top_docs",
  "rag_hybrid_candidates",
  "rag_hybrid_rrf_k",
  "rag_hybrid_weight_lexical",
  "rag_hybrid_weight_semantic",
  "rag_lexical_stop_words",
  "code_embed_base_url",
  "code_embed_model",
  "code_embed_dim",
  "code_embed_batch",
  "code_embed_concurrency",
  "code_embed_timeout",
  "code_rag_enabled",
  "code_rag_chunk_size",
  "code_rag_chunk_overlap",
  "code_rag_top_k",
  "code_rag_include",
  "code_rag_include_extra",
  "code_rag_exclude",
];

const LANGFUSE_FIELDS = [
  "langfuse_enabled",
  "langfuse_host",
  "langfuse_public_key",
  "langfuse_environment",
  "langfuse_timeout",
  "langfuse_capture",
  "langfuse_max_content_bytes",
  "langfuse_trace_docs",
  "langfuse_trace_embed",
  "langfuse_trace_mcp",
  "langfuse_trace_http",
];

function cloneValue(value) {
  if (Array.isArray(value)) return value.map(cloneValue);
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, nested]) => [key, cloneValue(nested)]));
  }
  return value;
}

function freezeDeep(value) {
  if (!value || typeof value !== "object" || Object.isFrozen(value)) return value;
  for (const nested of Object.values(value)) freezeDeep(nested);
  return Object.freeze(value);
}

function pickFields(source, fields) {
  return Object.fromEntries(fields.map((field) => [field, cloneValue(source[field])]));
}

export function normalizeSettingsSnapshot(config) {
  if (!config) return null;

  const snapshot = cloneValue(config);
  snapshot.repo_schedules = Array.isArray(snapshot.repo_schedules)
    ? snapshot.repo_schedules.map((schedule) => ({
        ...schedule,
        specs: Array.isArray(schedule.specs) ? schedule.specs : [],
      }))
    : [];
  snapshot.rag_lexical_stop_words = Array.isArray(snapshot.rag_lexical_stop_words)
    ? snapshot.rag_lexical_stop_words
    : [];
  if (typeof snapshot.rag_keep_markdown_targets !== "boolean") snapshot.rag_keep_markdown_targets = false;
  if (typeof snapshot.web_image_analysis_enabled !== "boolean") snapshot.web_image_analysis_enabled = false;
  if (typeof snapshot.web_image_allow_authenticated !== "boolean") snapshot.web_image_allow_authenticated = false;
  if (!snapshot.web_image_max_per_page) snapshot.web_image_max_per_page = 3;
  if (!snapshot.web_image_max_bytes) snapshot.web_image_max_bytes = 4 * 1024 * 1024;
  if (!snapshot.web_image_max_pixels) snapshot.web_image_max_pixels = 16000000;

  return freezeDeep(snapshot);
}

export function createRuntimeDraft(snapshot) {
  return pickFields(snapshot, [...RUNTIME_FIELDS, "webhook_secret_set"]);
}

export function createDocsDraft(snapshot) {
  const draft = pickFields(snapshot, [
    ...DOCS_FIELDS,
    "docs_default_prompt",
    "llm_api_key_set",
    "embed_api_key_set",
    "code_embed_api_key_set",
  ]);
  draft.rag_lexical_stop_words_text = (draft.rag_lexical_stop_words ?? []).join(", ");
  return draft;
}

export function createLangfuseDraft(snapshot) {
  return pickFields(snapshot, [...LANGFUSE_FIELDS, "langfuse_secret_key_set"]);
}

export function parseStopWords(text) {
  return (text ?? "")
    .split(",")
    .map((word) => word.trim())
    .filter(Boolean);
}

export function cleanSchedules(list) {
  return (list || [])
    .map((schedule) => ({
      namespace: (schedule.namespace || "").trim(),
      specs: (schedule.specs || []).map((spec) => (spec || "").trim()).filter(Boolean),
      disabled: !!schedule.disabled,
    }))
    .filter((schedule) => schedule.specs.length > 0);
}

export function buildRuntimePayload(draft, webhookSecret, clearWebhook = false) {
  const payload = pickFields(draft, RUNTIME_FIELDS);
  payload.task_concurrency = Number(draft.task_concurrency);
  payload.repo_schedules = cleanSchedules(draft.repo_schedules);
  if (clearWebhook) payload.webhook_secret = "";
  else if (webhookSecret) payload.webhook_secret = webhookSecret;
  return payload;
}

export function buildDocsPayload(draft, secrets = {}) {
  const payload = pickFields(draft, DOCS_FIELDS);
  payload.rag_lexical_stop_words = parseStopWords(draft.rag_lexical_stop_words_text);
  payload.llm_api_key = secrets.llm ?? "";
  payload.embed_api_key = secrets.embed ?? "";
  payload.code_embed_api_key = secrets.codeEmbed ?? "";
  return payload;
}

export function buildLangfusePayload(draft, secretKey = "") {
  const payload = pickFields(draft, LANGFUSE_FIELDS);
  payload.langfuse_enabled = !!draft.langfuse_enabled;
  payload.langfuse_host = draft.langfuse_host || "";
  payload.langfuse_public_key = draft.langfuse_public_key || "";
  payload.langfuse_secret_key = secretKey;
  payload.langfuse_environment = draft.langfuse_environment || "";
  payload.langfuse_capture = draft.langfuse_capture || "full";
  payload.langfuse_max_content_bytes = Number(draft.langfuse_max_content_bytes) || 0;
  payload.langfuse_trace_docs = !!draft.langfuse_trace_docs;
  payload.langfuse_trace_embed = !!draft.langfuse_trace_embed;
  payload.langfuse_trace_mcp = !!draft.langfuse_trace_mcp;
  payload.langfuse_trace_http = !!draft.langfuse_trace_http;
  return payload;
}
