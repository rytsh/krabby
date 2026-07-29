// Package langfuse exports krabby's LLM activity to a Langfuse instance as
// OpenTelemetry traces.
//
// It deliberately runs its own tracer provider rather than reusing the one
// tell installs globally, for two reasons:
//
//   - Langfuse ingests OTLP over HTTP only ("gRPC is not supported yet"),
//     while tell builds its exporter on otlptracegrpc. The two cannot share a
//     connection.
//   - Langfuse bills per observation. The global provider carries an HTTP
//     server span for every REST call and MCP poll; none of that belongs in an
//     LLM-observability backend.
//
// The provider is therefore isolated: nothing reaches Langfuse unless this
// package creates the span.
package langfuse

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/rytsh/krabby/internal/config"
)

// TracesPath is the full path traces are posted to, appended to the configured
// host. Exported so the settings connectivity test probes the same URL the
// exporter uses rather than a second guess at it.
//
// It has to be the signal-specific form. otlptracehttp.WithURLPath sets the
// request path outright — it does not append "/v1/traces" the way the
// OTEL_EXPORTER_OTLP_ENDPOINT environment variable and the OTel Collector's
// otlphttp exporter do. Langfuse documents the base endpoint
// ("/api/public/otel") for those, and the signal-specific endpoint
// ("/api/public/otel/v1/traces") for clients that configure a path directly.
// Posting to the base form returns 404.
const TracesPath = "/api/public/otel/v1/traces"

// tracerName identifies krabby's instrumentation scope in exported spans.
const tracerName = "github.com/rytsh/krabby"

// Export queue sizing.
//
// These are far below the OTel defaults (512 spans per batch, 2048 queued) and
// that is deliberate. krabby attaches whole prompts to its spans: a synthesis
// prompt reaches 256 KiB and a summary prompt 96 KiB. At the default batch
// size one export request would carry tens of megabytes, which Langfuse
// rejects; at the default queue depth the pending spans alone would outweigh
// the memory budget internal/memlimit hands the rest of the process.
const (
	maxExportBatchSize = 8
	maxQueueSize       = 256
	batchTimeout       = 5 * time.Second
)

// Scope identifies a part of krabby that can be traced independently. Each
// scope is switched on its own so an install can pay for exactly the
// visibility it wants.
type Scope uint8

const (
	// ScopeDocs covers the chat completions behind documentation generation.
	ScopeDocs Scope = 1 << iota
	// ScopeEmbed covers embedding calls made for docs and code RAG.
	ScopeEmbed
	// ScopeMCP covers MCP tool invocations, showing what a connected agent
	// actually asked for.
	ScopeMCP
	// ScopeHTTP covers REST API requests.
	ScopeHTTP
)

// Tracer creates Langfuse-shaped spans. The zero value is safe and inert; use
// Disabled() for an explicit no-op, or New to build a live one.
type Tracer struct {
	tracer   trace.Tracer
	provider *sdktrace.TracerProvider

	scopes  Scope
	capture config.Capture
	maxLen  int

	environment string
	release     string

	// fingerprint identifies the configuration this tracer was built from, so
	// a settings change that does not touch Langfuse can hand the live
	// provider to the next bundle instead of tearing down an exporter that
	// still has spans in flight.
	fingerprint string
}

// Disabled returns an inert tracer. Every method is safe to call on it and
// none of them allocate a real span, so callers never need a nil check.
func Disabled() *Tracer {
	return &Tracer{
		tracer:      noop.NewTracerProvider().Tracer(tracerName),
		fingerprint: "disabled",
	}
}

// New builds a live tracer from cfg. A disabled or incompletely configured
// section yields Disabled() and no error: observability must never be the
// reason krabby refuses to start.
func New(cfg config.Langfuse) (*Tracer, error) {
	if !cfg.Enabled {
		return Disabled(), nil
	}

	host := strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	if host == "" || cfg.PublicKey == "" || cfg.SecretKey == "" {
		return Disabled(), fmt.Errorf("langfuse enabled but host, public_key or secret_key is empty")
	}

	u, err := url.Parse(host)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Disabled(), fmt.Errorf("invalid langfuse host %q", cfg.Host)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(u.Host),
		otlptracehttp.WithURLPath(strings.TrimSuffix(u.Path, "/") + TracesPath),
		otlptracehttp.WithTimeout(timeout),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": "Basic " + base64.StdEncoding.EncodeToString(
				[]byte(cfg.PublicKey+":"+cfg.SecretKey)),
			// Without this header Langfuse routes directly ingested OTLP
			// through the legacy path, where new data can take ten minutes to
			// appear in the UI.
			"x-langfuse-ingestion-version": "4",
		}),
	}

	if u.Scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	// The exporter is created without a context deadline on purpose: it
	// connects lazily on first export, so New never blocks on a reachable
	// Langfuse.
	exp, err := otlptracehttp.New(context.Background(), opts...)
	if err != nil {
		return Disabled(), fmt.Errorf("create langfuse exporter; %w", err)
	}

	installErrorHandler()

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(config.ServiceName),
		semconv.ServiceVersion(config.Version),
	))
	if err != nil {
		// A schema mismatch is not worth failing startup over; the default
		// resource still identifies the process.
		res = resource.Default()
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxExportBatchSize(maxExportBatchSize),
			sdktrace.WithMaxQueueSize(maxQueueSize),
			sdktrace.WithBatchTimeout(batchTimeout),
		),
		// Second line of defence behind maxLen: a span that somehow carries an
		// oversized attribute is clipped by the SDK rather than by Langfuse
		// rejecting the whole batch.
		sdktrace.WithSpanLimits(spanLimits(cfg)),
	)

	return &Tracer{
		tracer:      provider.Tracer(tracerName),
		provider:    provider,
		scopes:      scopesOf(cfg),
		capture:     config.ParseCapture(string(cfg.Capture)),
		maxLen:      cfg.MaxContentBytes,
		environment: strings.TrimSpace(cfg.Environment),
		release:     config.Version,
		fingerprint: Fingerprint(cfg),
	}, nil
}

// errorHandlerOnce guards the one-time installation of the global handler.
var errorHandlerOnce sync.Once

// installErrorHandler routes OpenTelemetry's internal errors into krabby's
// logger.
//
// Export failures are reported through otel.Handle, whose default handler
// writes to the standard library logger — outside krabby's structured output
// and easy to miss entirely. A misconfigured endpoint or a rejected key then
// looks exactly like a working exporter that simply has nothing to say, which
// is the worst possible failure mode for telemetry: silence.
//
// The handler is global and therefore shared with the telemetry collector's
// exporter, so the message names neither.
func installErrorHandler() {
	errorHandlerOnce.Do(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			slog.Error("opentelemetry export failed", "error", err)
		}))
	})
}

func spanLimits(cfg config.Langfuse) sdktrace.SpanLimits {
	limits := sdktrace.NewSpanLimits()
	if cfg.MaxContentBytes > 0 {
		// Leave headroom for the JSON envelope the value is wrapped in.
		limits.AttributeValueLengthLimit = cfg.MaxContentBytes + 1024
	}

	return limits
}

func scopesOf(cfg config.Langfuse) Scope {
	var s Scope
	if cfg.TraceDocs {
		s |= ScopeDocs
	}
	if cfg.TraceEmbed {
		s |= ScopeEmbed
	}
	if cfg.TraceMCP {
		s |= ScopeMCP
	}
	if cfg.TraceHTTP {
		s |= ScopeHTTP
	}

	return s
}

// Fingerprint derives a stable identity for a Langfuse configuration. Two
// configurations with the same fingerprint produce an identical exporter, so
// the live one can be reused across a settings change instead of being torn
// down with spans still queued.
func Fingerprint(cfg config.Langfuse) string {
	if !cfg.Enabled {
		return "disabled"
	}

	// The secret is hashed rather than abbreviated. A prefix-and-length digest
	// would collide across a rotation (Langfuse keys share the "sk-lf-" prefix
	// and a fixed length), leaving the old exporter live with the old key.
	secret := "-"
	if cfg.SecretKey != "" {
		sum := sha256.Sum256([]byte(cfg.SecretKey))
		secret = hex.EncodeToString(sum[:8])
	}

	return strings.Join([]string{
		cfg.Host, cfg.PublicKey, secret, cfg.Environment,
		cfg.Timeout.String(),
		string(config.ParseCapture(string(cfg.Capture))),
		fmt.Sprint(cfg.MaxContentBytes),
		fmt.Sprint(scopesOf(cfg)),
	}, "|")
}

// Same reports whether t was built from a configuration equivalent to cfg.
func (t *Tracer) Same(cfg config.Langfuse) bool {
	return t != nil && t.fingerprint == Fingerprint(cfg)
}

// Enabled reports whether any span will actually be exported.
func (t *Tracer) Enabled() bool { return t != nil && t.provider != nil }

// On reports whether a scope is being traced. Call sites should check this
// before doing any work that exists only to populate a span.
func (t *Tracer) On(s Scope) bool {
	return t != nil && t.provider != nil && t.scopes&s != 0
}

// Shutdown flushes queued spans and releases the exporter. It is safe on a
// disabled tracer.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t == nil || t.provider == nil {
		return nil
	}

	if err := t.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown langfuse tracer; %w", err)
	}

	return nil
}

// Capture returns the configured content capture mode.
func (t *Tracer) Capture() config.Capture {
	if t == nil {
		return config.CaptureOff
	}

	return t.capture
}

// text prepares a value for export under the configured capture policy.
// The empty string means "do not attach".
func (t *Tracer) text(s string) string {
	if t == nil || t.capture == config.CaptureOff || s == "" {
		return ""
	}

	limit := t.maxLen
	if t.capture == config.CaptureTruncated {
		if limit <= 0 || limit > truncatedBudget {
			limit = truncatedBudget
		}
	}

	return clip(s, limit)
}

// truncatedBudget is the byte budget of the "truncated" capture mode. Large
// enough to hold a system prompt and the head of the payload it applies to,
// small enough that a forty-group docs build does not push megabytes per
// repository.
const truncatedBudget = 8 << 10

// clip shortens s to limit bytes on a rune boundary, marking that it was cut.
// limit <= 0 means no limit.
func clip(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}

	cut := limit
	// Do not split a multi-byte rune: back up to the start of the last one.
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}

	return s[:cut] + fmt.Sprintf("\n…[truncated, %d of %d bytes]", cut, len(s))
}

// utf8Start reports whether b begins a UTF-8 encoded rune.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// jsonOr renders v as JSON, falling back to its Go representation so a span is
// never dropped because of an unmarshalable value.
func jsonOr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}

	return string(b)
}

// baseAttrs are the attributes every span carries so Langfuse can filter and
// aggregate across observations rather than only at trace level.
func (t *Tracer) baseAttrs() []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2)
	if t.environment != "" {
		attrs = append(attrs, attribute.String(attrEnvironment, t.environment))
	}
	if t.release != "" {
		attrs = append(attrs, attribute.String(attrRelease, t.release))
	}

	return attrs
}

// endWithError closes a span, mapping a Go error onto the status Langfuse
// reads as an observation level.
func endWithError(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	} else {
		span.SetStatus(codes.Ok, "")
	}

	span.End()
}
