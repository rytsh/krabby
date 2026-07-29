package langfuse

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TraceInfo describes a unit of work that becomes one Langfuse trace.
type TraceInfo struct {
	// Name is the trace name shown in the Langfuse UI.
	Name string
	// SessionID groups related traces. krabby uses the repository id, so every
	// build of one repository lines up in a single session.
	SessionID string
	// UserID is optional and normally empty: krabby's LLM work is not
	// user-initiated.
	UserID string
	// Tags label the trace for filtering.
	Tags []string
	// Metadata becomes filterable top-level keys on the trace.
	Metadata map[string]string
	// Input is optional context for the whole trace.
	Input string
}

// GenerationInfo describes one model call.
type GenerationInfo struct {
	// Name is the observation name, e.g. "chat.summary".
	Name string
	// System identifies the provider ("openai", "ollama", ...).
	System string
	// Operation is the GenAI operation name ("chat" or "embeddings").
	Operation string
	// Model is the model requested.
	Model string
	// Input is the prompt, already in whatever shape should be shown.
	Input any
	// ModelParameters are the invocation settings worth recording.
	ModelParameters map[string]any
	// Metadata becomes filterable keys on the observation.
	Metadata map[string]string
}

// GenerationResult closes out a model call.
type GenerationResult struct {
	// Output is the model's reply.
	Output any
	// InputTokens and OutputTokens are the provider's accounting; zero when
	// it reported none.
	InputTokens  int
	OutputTokens int
	// ResponseModel is the model that actually answered, when the provider
	// echoes one back.
	ResponseModel string
	// FirstTokenAt is when the first content token arrived, which Langfuse
	// renders as time-to-first-token. Zero when not streamed.
	FirstTokenAt time.Time
	// Attempts counts HTTP requests including retries.
	Attempts int
	// Err marks the call as failed.
	Err error
}

// EndTrace closes a trace started by StartTrace. output may be nil.
type EndTrace func(output any, err error)

// ctxKey marks a context as already carrying a span from this provider.
type ctxKey struct{}

// marked reports whether ctx already holds a Langfuse span.
//
// This cannot be answered from the OpenTelemetry context alone. krabby runs two
// tracer providers — tell's gRPC one for the collector, and this one — and the
// HTTP middleware puts a span from the *other* provider on the context of every
// request. Parenting to it would produce a span whose parent id Langfuse never
// receives, and Langfuse does not materialise a trace without its root: the
// observation would silently vanish. A marker of our own is the only reliable
// way to tell "there is a parent Langfuse span here" from "there is a foreign
// span here".
func marked(ctx context.Context) bool {
	return ctx.Value(ctxKey{}) != nil
}

func mark(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, struct{}{})
}

// rootUnlessNested returns the span options for a span that should attach to a
// Langfuse parent when there is one, and start its own trace when there is not.
func rootUnlessNested(ctx context.Context, opts ...trace.SpanStartOption) []trace.SpanStartOption {
	if marked(ctx) {
		return opts
	}

	return append(opts, trace.WithNewRoot())
}

// EndGeneration closes a generation started by StartGeneration.
type EndGeneration func(GenerationResult)

// StartTrace opens a root span for one unit of work.
//
// The span is always a new root, never a child of whatever is already on ctx.
// That matters: a docs build triggered over REST inherits the HTTP server span
// created by the telemetry middleware, and that span goes to the gRPC
// collector, not to Langfuse. Parenting to it would produce a trace whose root
// Langfuse never receives, and Langfuse does not materialise a trace without
// its root.
//
// When the scope is off the returned context is unchanged and the returned
// closer does nothing.
func (t *Tracer) StartTrace(ctx context.Context, scope Scope, info TraceInfo) (context.Context, EndTrace) {
	if !t.On(scope) {
		return ctx, func(any, error) {}
	}

	attrs := append(t.baseAttrs(),
		attribute.String(attrObsType, obsTypeSpan),
		attribute.String(attrTraceName, info.Name),
	)

	if info.SessionID != "" {
		attrs = append(attrs, attribute.String(attrSessionID, info.SessionID))
	}
	if info.UserID != "" {
		attrs = append(attrs, attribute.String(attrUserID, info.UserID))
	}
	if len(info.Tags) > 0 {
		attrs = append(attrs, attribute.StringSlice(attrTraceTags, info.Tags))
	}

	for k, v := range info.Metadata {
		attrs = append(attrs, attribute.String(attrTraceMetaPfx+k, v))
	}

	if in := t.text(info.Input); in != "" {
		attrs = append(attrs, attribute.String(attrObsInput, in))
	}

	ctx, span := t.tracer.Start(mark(ctx), info.Name,
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)

	return ctx, func(output any, err error) {
		if output != nil {
			if out := t.text(jsonOr(output)); out != "" {
				span.SetAttributes(attribute.String(attrObsOutput, out))
			}
		}

		endWithError(span, err)
	}
}

// StartGeneration opens a child span typed as a Langfuse generation, so the UI
// shows it with model, tokens and cost.
func (t *Tracer) StartGeneration(ctx context.Context, scope Scope, info GenerationInfo) (context.Context, EndGeneration) {
	if !t.On(scope) {
		return ctx, func(GenerationResult) {}
	}

	attrs := append(t.baseAttrs(),
		attribute.String(attrObsType, obsTypeGeneration),
		attribute.String(attrObsModel, info.Model),
		attribute.String(attrGenAIReqModel, info.Model),
	)

	if info.System != "" {
		attrs = append(attrs, attribute.String(attrGenAISystem, info.System))
	}
	if info.Operation != "" {
		attrs = append(attrs, attribute.String(attrGenAIOperation, info.Operation))
	}
	if len(info.ModelParameters) > 0 {
		attrs = append(attrs, attribute.String(attrObsModelParam, jsonOr(info.ModelParameters)))
	}

	for k, v := range info.Metadata {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+k, v))
	}

	if info.Input != nil {
		if in := t.text(jsonOr(info.Input)); in != "" {
			attrs = append(attrs, attribute.String(attrObsInput, in))
		}
	}

	// A generation is normally nested under the unit of work that issued it,
	// but it must stand on its own when nothing above it created a Langfuse
	// span — a search served over REST, or an indexing run on the background
	// queue, reaches the embedder with no krabby root in scope.
	ctx, span := t.tracer.Start(mark(ctx), info.Name,
		rootUnlessNested(ctx,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attrs...),
		)...,
	)

	start := time.Now()

	return ctx, func(res GenerationResult) {
		done := make([]attribute.KeyValue, 0, 8)

		if res.Output != nil {
			if out := t.text(jsonOr(res.Output)); out != "" {
				done = append(done, attribute.String(attrObsOutput, out))
			}
		}

		// Langfuse reads usage_details as a JSON object and derives cost from
		// it together with the model name. Emitting it only when the provider
		// actually reported something keeps a zero from being mistaken for a
		// free call.
		if res.InputTokens > 0 || res.OutputTokens > 0 {
			done = append(done,
				attribute.String(attrObsUsage, jsonOr(map[string]int{
					"input":  res.InputTokens,
					"output": res.OutputTokens,
					"total":  res.InputTokens + res.OutputTokens,
				})),
				attribute.Int(attrGenAIInputToken, res.InputTokens),
				attribute.Int(attrGenAIOutputToken, res.OutputTokens),
			)
		}

		if res.ResponseModel != "" {
			done = append(done, attribute.String(attrGenAIRespModel, res.ResponseModel))
		}

		if !res.FirstTokenAt.IsZero() && !res.FirstTokenAt.Before(start) {
			done = append(done, attribute.String(attrObsFirstToken,
				res.FirstTokenAt.UTC().Format(time.RFC3339Nano)))
		}

		if res.Attempts > 1 {
			// Retries are folded into the one observation rather than emitted
			// as siblings: a rate-limited build would otherwise triple its
			// observation count for no added insight.
			done = append(done, attribute.String(attrObsMetaPrefix+"attempts",
				jsonOr(res.Attempts)))
		}

		span.SetAttributes(done...)
		endWithError(span, res.Err)
	}
}

// StartSpan opens a plain child span (a Langfuse observation of type "span"),
// for work that is not itself a model call.
func (t *Tracer) StartSpan(ctx context.Context, scope Scope, name string, input any) (context.Context, func(output any, err error)) {
	if !t.On(scope) {
		return ctx, func(any, error) {}
	}

	attrs := append(t.baseAttrs(), attribute.String(attrObsType, obsTypeSpan))

	if input != nil {
		if in := t.text(jsonOr(input)); in != "" {
			attrs = append(attrs, attribute.String(attrObsInput, in))
		}
	}

	ctx, span := t.tracer.Start(mark(ctx), name,
		rootUnlessNested(ctx,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attrs...),
		)...,
	)

	return ctx, func(output any, err error) {
		if output != nil {
			if out := t.text(jsonOr(output)); out != "" {
				span.SetAttributes(attribute.String(attrObsOutput, out))
			}
		}

		endWithError(span, err)
	}
}

// SystemFromBaseURL guesses the gen_ai.system value from an endpoint URL, so
// traces group by provider without another configuration field. Everything
// OpenAI-compatible that is not recognised reports "openai-compatible" rather
// than claiming to be OpenAI.
func SystemFromBaseURL(baseURL string) string {
	host := strings.ToLower(baseURL)

	switch {
	case strings.Contains(host, "openai.azure.com"):
		return "az.ai.openai"
	case strings.Contains(host, "api.openai.com"):
		return "openai"
	case strings.Contains(host, "generativelanguage.googleapis.com"),
		strings.Contains(host, "gemini"):
		return "gcp.gemini"
	case strings.Contains(host, "api.anthropic.com"):
		return "anthropic"
	case strings.Contains(host, "api.mistral.ai"):
		return "mistral_ai"
	case strings.Contains(host, "api.deepseek.com"):
		return "deepseek"
	case strings.Contains(host, "api.groq.com"):
		return "groq"
	case strings.Contains(host, "11434"), strings.Contains(host, "ollama"):
		return "ollama"
	case strings.Contains(host, "1234"), strings.Contains(host, "lmstudio"):
		return "lm_studio"
	default:
		return "openai-compatible"
	}
}
