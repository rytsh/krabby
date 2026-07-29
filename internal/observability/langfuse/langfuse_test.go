package langfuse

import (
	"context"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/rytsh/krabby/internal/config"
)

func TestDisabledTracerIsInert(t *testing.T) {
	tr := Disabled()

	if tr.Enabled() {
		t.Fatal("disabled tracer reports enabled")
	}

	for _, scope := range []Scope{ScopeDocs, ScopeEmbed, ScopeMCP, ScopeHTTP} {
		if tr.On(scope) {
			t.Fatalf("disabled tracer reports scope %d on", scope)
		}
	}

	// Every entry point must be callable without a provider: call sites are
	// written without nil guards on purpose.
	ctx, endTrace := tr.StartTrace(context.Background(), ScopeDocs, TraceInfo{Name: "x"})
	endTrace(nil, nil)

	_, endGen := tr.StartGeneration(ctx, ScopeDocs, GenerationInfo{Name: "y"})
	endGen(GenerationResult{})

	_, endSpan := tr.StartSpan(ctx, ScopeMCP, "z", nil)
	endSpan(nil, nil)

	if err := tr.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown disabled tracer: %v", err)
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]config.Langfuse{
		"no host":   {Enabled: true, PublicKey: "pk", SecretKey: "sk"},
		"no public": {Enabled: true, Host: "https://cloud.langfuse.com", SecretKey: "sk"},
		"no secret": {Enabled: true, Host: "https://cloud.langfuse.com", PublicKey: "pk"},
		"bad host":  {Enabled: true, Host: "not a url", PublicKey: "pk", SecretKey: "sk"},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			tr, err := New(cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			// The tracer must still be usable: a misconfigured export cannot
			// be allowed to take the process down.
			if tr == nil || tr.Enabled() {
				t.Fatal("expected an inert tracer alongside the error")
			}
		})
	}
}

func TestNewDisabledIsNotAnError(t *testing.T) {
	tr, err := New(config.Langfuse{Enabled: false, Host: "nonsense"})
	if err != nil {
		t.Fatalf("disabled config returned an error: %v", err)
	}
	if tr.Enabled() {
		t.Fatal("disabled config produced a live tracer")
	}
}

func TestScopeGating(t *testing.T) {
	cfg := config.Langfuse{
		Enabled:    true,
		Host:       "http://localhost:3000",
		PublicKey:  "pk",
		SecretKey:  "sk",
		TraceDocs:  true,
		TraceEmbed: false,
	}

	tr, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = tr.Shutdown(context.Background()) })

	if !tr.On(ScopeDocs) {
		t.Error("docs scope should be on")
	}
	if tr.On(ScopeEmbed) {
		t.Error("embed scope should be off")
	}
	if tr.On(ScopeMCP) || tr.On(ScopeHTTP) {
		t.Error("unset scopes should be off")
	}
}

// A settings change that leaves the observability configuration alone must
// produce the same fingerprint, so the live exporter is reused instead of being
// torn down with spans still queued.
func TestFingerprintStability(t *testing.T) {
	base := config.Langfuse{
		Enabled: true, Host: "https://cloud.langfuse.com",
		PublicKey: "pk-lf-1", SecretKey: "sk-lf-abcdefgh",
		Environment: "production", Timeout: 10 * time.Second,
		Capture: config.CaptureFull, MaxContentBytes: 1 << 20,
		TraceDocs: true,
	}

	if Fingerprint(base) != Fingerprint(base) {
		t.Fatal("fingerprint is not deterministic")
	}

	changed := base
	changed.SecretKey = "sk-lf-zzzzzzzz"
	if Fingerprint(base) == Fingerprint(changed) {
		t.Error("rotating the secret must change the fingerprint")
	}

	changed = base
	changed.TraceEmbed = true
	if Fingerprint(base) == Fingerprint(changed) {
		t.Error("changing a scope must change the fingerprint")
	}

	changed = base
	changed.Capture = config.CaptureOff
	if Fingerprint(base) == Fingerprint(changed) {
		t.Error("changing capture must change the fingerprint")
	}

	// Disabled collapses to one identity regardless of leftover fields, so
	// editing a host while the export is off does not churn the exporter.
	off1 := config.Langfuse{Enabled: false, Host: "a"}
	off2 := config.Langfuse{Enabled: false, Host: "b", SecretKey: "sk"}
	if Fingerprint(off1) != Fingerprint(off2) {
		t.Error("disabled configurations must share a fingerprint")
	}
}

// The fingerprint travels in memory and can reach a debug dump; it must not
// carry a usable secret.
func TestFingerprintDoesNotLeakSecret(t *testing.T) {
	secret := "sk-lf-supersecretvalue"
	fp := Fingerprint(config.Langfuse{
		Enabled: true, Host: "h", PublicKey: "pk", SecretKey: secret,
	})

	if strings.Contains(fp, secret) {
		t.Fatalf("fingerprint contains the full secret: %q", fp)
	}
}

func TestClip(t *testing.T) {
	if got := clip("hello", 0); got != "hello" {
		t.Errorf("limit 0 must not clip, got %q", got)
	}
	if got := clip("hello", 10); got != "hello" {
		t.Errorf("short input must not clip, got %q", got)
	}

	got := clip("hello world", 5)
	if !strings.HasPrefix(got, "hello") {
		t.Errorf("clip lost the head: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("clip did not mark the cut: %q", got)
	}

	// Cutting mid-rune would produce invalid UTF-8 in the exported attribute.
	multi := strings.Repeat("ü", 10) // 2 bytes each
	cut := clip(multi, 5)
	head := strings.SplitN(cut, "\n", 2)[0]
	if strings.ContainsRune(head, '\uFFFD') {
		t.Errorf("clip split a rune: %q", head)
	}
}

func TestCapturePolicy(t *testing.T) {
	long := strings.Repeat("x", 100<<10)

	t.Run("off attaches nothing", func(t *testing.T) {
		tr := &Tracer{capture: config.CaptureOff, maxLen: 0}
		if got := tr.text(long); got != "" {
			t.Errorf("capture off returned %d bytes", len(got))
		}
	})

	t.Run("truncated honours its own budget", func(t *testing.T) {
		tr := &Tracer{capture: config.CaptureTruncated, maxLen: 1 << 20}
		got := tr.text(long)
		if len(got) > truncatedBudget+128 {
			t.Errorf("truncated returned %d bytes, budget is %d", len(got), truncatedBudget)
		}
	})

	t.Run("full still respects the ceiling", func(t *testing.T) {
		tr := &Tracer{capture: config.CaptureFull, maxLen: 1 << 10}
		got := tr.text(long)
		if len(got) > (1<<10)+128 {
			t.Errorf("full ignored the byte ceiling: %d bytes", len(got))
		}
	})

	t.Run("full with no ceiling passes through", func(t *testing.T) {
		tr := &Tracer{capture: config.CaptureFull, maxLen: 0}
		if got := tr.text(long); len(got) != len(long) {
			t.Errorf("expected passthrough, got %d of %d bytes", len(got), len(long))
		}
	})
}

func TestSystemFromBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com/v1":      "openai",
		"http://localhost:11434/v1":      "ollama",
		"https://api.mistral.ai/v1":      "mistral_ai",
		"https://api.anthropic.com":      "anthropic",
		"https://my-gateway.internal/v1": "openai-compatible",
	}

	for url, want := range cases {
		if got := SystemFromBaseURL(url); got != want {
			t.Errorf("SystemFromBaseURL(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestParseCapture(t *testing.T) {
	cases := map[string]config.Capture{
		"off":         config.CaptureOff,
		"OFF":         config.CaptureOff,
		" truncated ": config.CaptureTruncated,
		"full":        config.CaptureFull,
		// An unknown or empty value keeps the documented default rather than
		// silently disabling capture.
		"":         config.CaptureFull,
		"nonsense": config.CaptureFull,
	}

	for in, want := range cases {
		if got := config.ParseCapture(in); got != want {
			t.Errorf("ParseCapture(%q) = %q, want %q", in, got, want)
		}
	}
}

// liveTracer builds a real tracer pointed at a dead endpoint. Spans are never
// exported, but they are recorded, which is all these tests need.
func liveTracer(t *testing.T, scopes func(*config.Langfuse)) *Tracer {
	t.Helper()

	cfg := config.Langfuse{
		Enabled: true, Host: "http://127.0.0.1:1", PublicKey: "pk", SecretKey: "sk",
		TraceDocs: true, TraceEmbed: true, TraceMCP: true, TraceHTTP: true,
	}
	if scopes != nil {
		scopes(&cfg)
	}

	tr, err := New(cfg)
	if err != nil {
		t.Fatalf("new tracer: %v", err)
	}
	t.Cleanup(func() { _ = tr.Shutdown(context.Background()) })

	return tr
}

// A generation reached with no krabby span above it must start its own trace.
//
// This is the REST-search and background-indexing case: the embedder is called
// with a context that may carry a span from the *other* tracer provider (the
// telemetry collector). Inheriting it would produce a parent id Langfuse never
// receives, and Langfuse creates no trace without its root, so the observation
// would disappear.
func TestGenerationWithoutParentStartsItsOwnTrace(t *testing.T) {
	tr := liveTracer(t, nil)

	// A span from a foreign provider, exactly as the HTTP telemetry middleware
	// leaves on the request context.
	foreign := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = foreign.Shutdown(context.Background()) })

	ctx, foreignSpan := foreign.Tracer("other").Start(context.Background(), "http.server")
	defer foreignSpan.End()

	foreignTrace := foreignSpan.SpanContext().TraceID()
	if !foreignSpan.SpanContext().IsValid() {
		t.Fatal("test setup: the foreign span is not recording")
	}

	genCtx, end := tr.StartGeneration(ctx, ScopeEmbed, GenerationInfo{Name: "embeddings"})
	defer end(GenerationResult{})

	got := trace.SpanContextFromContext(genCtx)
	if !got.IsValid() {
		t.Fatal("generation span is not valid")
	}

	if got.TraceID() == foreignTrace {
		t.Fatal("generation attached to the foreign provider's trace; Langfuse would drop it as an orphan")
	}
}

// When krabby did create a root, the generation must nest under it rather than
// splitting off into a trace of its own.
func TestGenerationNestsUnderKrabbyRoot(t *testing.T) {
	tr := liveTracer(t, nil)

	rootCtx, endRoot := tr.StartTrace(context.Background(), ScopeDocs, TraceInfo{Name: "docs.generate"})
	defer endRoot(nil, nil)

	rootTrace := trace.SpanContextFromContext(rootCtx).TraceID()

	genCtx, end := tr.StartGeneration(rootCtx, ScopeDocs, GenerationInfo{Name: "chat.summary"})
	defer end(GenerationResult{})

	if got := trace.SpanContextFromContext(genCtx).TraceID(); got != rootTrace {
		t.Fatalf("generation started a new trace (%s) instead of nesting under the root (%s)", got, rootTrace)
	}
}

// A root is a root even when a foreign span is in scope.
func TestStartTraceIgnoresForeignParent(t *testing.T) {
	tr := liveTracer(t, nil)

	foreign := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = foreign.Shutdown(context.Background()) })

	ctx, foreignSpan := foreign.Tracer("other").Start(context.Background(), "http.server")
	defer foreignSpan.End()

	rootCtx, end := tr.StartTrace(ctx, ScopeMCP, TraceInfo{Name: "search_code"})
	defer end(nil, nil)

	if trace.SpanContextFromContext(rootCtx).TraceID() == foreignSpan.SpanContext().TraceID() {
		t.Fatal("StartTrace inherited the foreign trace")
	}
}

// A scope that is off must not mark the context, or the next span would think
// it has a Langfuse parent and attach to nothing.
func TestDisabledScopeDoesNotMarkContext(t *testing.T) {
	tr := liveTracer(t, func(c *config.Langfuse) { c.TraceHTTP = false })

	// The HTTP scope is off, so this is a no-op and leaves ctx untouched.
	ctx, endHTTP := tr.StartTrace(context.Background(), ScopeHTTP, TraceInfo{Name: "GET /api/v1/docs/search"})
	defer endHTTP(nil, nil)

	if marked(ctx) {
		t.Fatal("a skipped span marked the context")
	}

	// The embedding underneath must therefore still become its own root.
	genCtx, end := tr.StartGeneration(ctx, ScopeEmbed, GenerationInfo{Name: "embeddings"})
	defer end(GenerationResult{})

	if !trace.SpanContextFromContext(genCtx).IsValid() {
		t.Fatal("generation span is not valid")
	}
}
