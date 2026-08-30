const OVERRIDE_DEFAULTS = {
  include: "",
  include_extra: "",
  exclude: "",
  graph_exclude: "",
  docs_prompt: "",
  docs_prompt_extra: "",
  docs_max_source_bytes: "",
  docs_max_group_bytes: "",
  docs_max_synthesis_bytes: "",
  skip_stages: [],
};

export const skippableRepoStages = [
  { key: "graph", label: "Knowledge graph" },
  { key: "code_index", label: "Semantic code index" },
  { key: "docs", label: "Documentation" },
  { key: "docs_index", label: "Documentation index" },
];

function commaList(value) {
  return (value || []).join(", ");
}

function splitCommaList(value) {
  return value
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

function positiveInt(value) {
  const parsed = Number.parseInt(String(value).trim(), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

export function createRepoOverridesDraft(repo) {
  const overrides = repo?.overrides || {};
  return {
    ...OVERRIDE_DEFAULTS,
    include: commaList(overrides.include),
    include_extra: commaList(overrides.include_extra),
    exclude: commaList(overrides.exclude),
    graph_exclude: commaList(overrides.graph_exclude),
    docs_prompt: overrides.docs_prompt || "",
    docs_prompt_extra: overrides.docs_prompt_extra || "",
    docs_max_source_bytes: overrides.docs_max_source_bytes ? String(overrides.docs_max_source_bytes) : "",
    docs_max_group_bytes: overrides.docs_max_group_bytes ? String(overrides.docs_max_group_bytes) : "",
    docs_max_synthesis_bytes: overrides.docs_max_synthesis_bytes ? String(overrides.docs_max_synthesis_bytes) : "",
    skip_stages: [...(overrides.skip_stages || [])],
  };
}

export function buildRepoOverridesPayload(draft) {
  return {
    include: splitCommaList(draft.include),
    include_extra: splitCommaList(draft.include_extra),
    exclude: splitCommaList(draft.exclude),
    graph_exclude: splitCommaList(draft.graph_exclude),
    docs_prompt: draft.docs_prompt.trim(),
    docs_prompt_extra: draft.docs_prompt_extra.trim(),
    docs_max_source_bytes: positiveInt(draft.docs_max_source_bytes),
    docs_max_group_bytes: positiveInt(draft.docs_max_group_bytes),
    docs_max_synthesis_bytes: positiveInt(draft.docs_max_synthesis_bytes),
    skip_stages: [...draft.skip_stages],
  };
}

export function hasRepoOverrides(repo) {
  const overrides = repo?.overrides || {};
  return Boolean(
      overrides.include?.length ||
      overrides.include_extra?.length ||
      overrides.exclude?.length ||
      overrides.graph_exclude?.length ||
      overrides.docs_prompt ||
      overrides.docs_prompt_extra ||
      overrides.docs_max_source_bytes ||
      overrides.docs_max_group_bytes ||
      overrides.docs_max_synthesis_bytes ||
      overrides.skip_stages?.length,
  );
}

export function repoSettingsRows(settings) {
  if (!settings) return [];
  const effective = settings.effective || {};
  const overrides = settings.overrides || {};
  const list = (value) => (value?.length ? value.join(", ") : "");
  const kibibytes = (value) => (value ? `${Math.round(value / 1024)} KiB` : "");

  return [
    {
      label: "Indexed files",
      value: effective.code_include_is_default ? "built-in allowlist" : list(effective.code_include),
      overridden: Boolean(overrides.include?.length),
    },
    {
      label: "Also indexed",
      value: list(effective.code_include_extra),
      overridden: Boolean(overrides.include_extra?.length),
    },
    { label: "Skipped", value: list(effective.code_exclude), overridden: Boolean(overrides.exclude?.length) },
    {
      label: "Graph ignores",
      value: list(effective.graph_exclude),
      overridden: Boolean(overrides.graph_exclude?.length),
    },
    {
      label: "Skipped stages",
      value: list(effective.skip_stages),
      overridden: Boolean(overrides.skip_stages?.length),
    },
    {
      label: "Docs input budget",
      value: effective.docs_limits ? `${kibibytes(effective.docs_limits.max_source_bytes)} per file` : "",
      overridden: Boolean(overrides.docs_max_source_bytes),
    },
    {
      label: "Docs prompt",
      value: effective.docs_prompt_source,
      overridden: effective.docs_prompt_source === "repo",
    },
    {
      label: "Extra doc rules",
      value: effective.docs_prompt_extras?.length ? effective.docs_prompt_extras.join(" + ") : "none",
      overridden: (effective.docs_prompt_extras || []).includes("repo"),
    },
  ].filter((row) => row.value);
}
