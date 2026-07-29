package langfuse

// Attribute keys Langfuse maps onto its own data model.
//
// The langfuse.* namespace takes precedence over the generic OpenTelemetry
// GenAI conventions, and is what Langfuse recommends for hand-instrumented
// code. The gen_ai.* keys are set alongside them so the same spans stay
// readable in a plain OTel backend, and so a future collector can route them
// without rewriting attributes.
const (
	// Trace level. Langfuse accepts these on any span in the trace, but only
	// applies them reliably to filters when every span carries them.
	attrTraceName     = "langfuse.trace.name"
	attrTraceTags     = "langfuse.trace.tags"
	attrSessionID     = "langfuse.session.id"
	attrUserID        = "langfuse.user.id"
	attrRelease       = "langfuse.release"
	attrEnvironment   = "langfuse.environment"
	attrTraceMetaPfx  = "langfuse.trace.metadata."
	attrObsMetaPrefix = "langfuse.observation.metadata."

	// Observation level.
	// Observation level and status message are deliberately absent: Langfuse
	// infers both from the OTel span status, which endWithError already sets.
	attrObsType       = "langfuse.observation.type"
	attrObsInput      = "langfuse.observation.input"
	attrObsOutput     = "langfuse.observation.output"
	attrObsModel      = "langfuse.observation.model.name"
	attrObsModelParam = "langfuse.observation.model.parameters"
	attrObsUsage      = "langfuse.observation.usage_details"
	attrObsFirstToken = "langfuse.observation.completion_start_time"

	// OpenTelemetry GenAI semantic conventions, set in parallel.
	attrGenAISystem      = "gen_ai.system"
	attrGenAIOperation   = "gen_ai.operation.name"
	attrGenAIReqModel    = "gen_ai.request.model"
	attrGenAIRespModel   = "gen_ai.response.model"
	attrGenAIInputToken  = "gen_ai.usage.input_tokens"
	attrGenAIOutputToken = "gen_ai.usage.output_tokens"
)

// Observation types Langfuse recognises.
const (
	obsTypeSpan       = "span"
	obsTypeGeneration = "generation"
)
