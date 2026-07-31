package settings

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/config"
)

func TestPatchApplyPreservesOmittedFields(t *testing.T) {
	t.Parallel()

	base := Settings{
		DocsEnabled:  true,
		LLMBaseURL:   "https://llm.example/v1",
		EmbedBaseURL: "https://embed.example/v1",
		EmbedModel:   "docs-model",
		RAGEnabled:   true,
		RAGTopK:      20,
	}

	var patch Patch
	if err := json.Unmarshal([]byte(`{"code_rag_enabled":true,"code_rag_top_k":7}`), &patch); err != nil {
		t.Fatal(err)
	}

	got := patch.Apply(base)
	if !got.CodeRAGEnabled || got.CodeRAGTopK != 7 {
		t.Errorf("code patch not applied: %#v", got)
	}

	if !got.DocsEnabled || !got.RAGEnabled || got.RAGTopK != 20 || got.EmbedModel != "docs-model" {
		t.Errorf("omitted fields changed: %#v", got)
	}
}

func TestSetPreservesInternalDocsIndexProjection(t *testing.T) {
	t.Parallel()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := New(db, Settings{DocsIndexProjection: 7})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Set(context.Background(), Settings{RAGTopK: 12})
	if err != nil {
		t.Fatal(err)
	}
	if got.DocsIndexProjection != 7 {
		t.Fatalf("docs index projection = %d, want 7", got.DocsIndexProjection)
	}
}

func TestRuntimePatch(t *testing.T) {
	t.Parallel()

	var patch Patch
	if err := json.Unmarshal([]byte(`{"git_poll_interval":3600000000000,"webhook_secret":"new"}`), &patch); err != nil {
		t.Fatal(err)
	}
	if !patch.RuntimeOnly() {
		t.Fatal("runtime-only patch was not recognized")
	}

	got := patch.Apply(Settings{WebhookSecret: "old"})
	if got.GitPollInterval != time.Hour || got.WebhookSecret != "new" {
		t.Fatalf("runtime patch result = %#v", got)
	}

	// Explicit empty clears the secret; omission preserves it.
	if err := json.Unmarshal([]byte(`{"webhook_secret":""}`), &patch); err != nil {
		t.Fatal(err)
	}
	if got := patch.Apply(Settings{WebhookSecret: "old"}); got.WebhookSecret != "" {
		t.Fatalf("explicit empty did not clear secret: %#v", got)
	}

	var docsPatch Patch
	if err := json.Unmarshal([]byte(`{"rag_enabled":true}`), &docsPatch); err != nil {
		t.Fatal(err)
	}
	if docsPatch.RuntimeOnly() {
		t.Fatal("docs patch incorrectly recognized as runtime-only")
	}
}

func TestEffectiveSchedulesFallback(t *testing.T) {
	t.Parallel()

	// Negative interval disables polling entirely.
	if got := (Settings{GitPollInterval: -1}).EffectiveSchedules(); got != nil {
		t.Fatalf("disabled interval should yield no schedules, got %#v", got)
	}

	// Zero maps to the hourly default across all namespaces.
	got := (Settings{GitPollInterval: 0}).EffectiveSchedules()
	if len(got) != 1 || got[0].Namespace != "*" || len(got[0].Specs) != 1 || got[0].Specs[0] != "@every 1h0m0s" {
		t.Fatalf("zero interval fallback = %#v", got)
	}

	// A positive interval maps to an @every spec.
	got = (Settings{GitPollInterval: 15 * time.Minute}).EffectiveSchedules()
	if len(got) != 1 || got[0].Specs[0] != "@every 15m0s" {
		t.Fatalf("positive interval fallback = %#v", got)
	}

	// Configured schedules take precedence over the interval fallback.
	cfg := Settings{
		GitPollInterval: time.Hour,
		RepoSchedules:   []RepoSchedule{{Namespace: "team-a", Specs: []string{"*/15 * * * *"}}},
	}
	got = cfg.EffectiveSchedules()
	if len(got) != 1 || got[0].Namespace != "team-a" {
		t.Fatalf("configured schedules not authoritative = %#v", got)
	}
}

func TestValidateSchedules(t *testing.T) {
	t.Parallel()

	ok := Settings{RepoSchedules: []RepoSchedule{
		{Namespace: "*", Specs: []string{"0 */6 * * *", "@every 30m"}},
	}}
	if err := ok.ValidateSchedules(); err != nil {
		t.Fatalf("valid specs rejected: %v", err)
	}

	bad := Settings{RepoSchedules: []RepoSchedule{
		{Namespace: "team-a", Specs: []string{"not a cron"}},
	}}
	if err := bad.ValidateSchedules(); err == nil {
		t.Fatal("invalid cron spec was not rejected")
	}

	empty := Settings{RepoSchedules: []RepoSchedule{{Namespace: "team-a", Specs: []string{"  "}}}}
	if err := empty.ValidateSchedules(); err == nil {
		t.Fatal("empty cron spec was not rejected")
	}
}

func TestRuntimePatchRepoSchedules(t *testing.T) {
	t.Parallel()

	var patch Patch
	if err := json.Unmarshal([]byte(`{"repo_schedules":[{"namespace":"*","specs":["0 * * * *"]}]}`), &patch); err != nil {
		t.Fatal(err)
	}
	if !patch.RuntimeOnly() {
		t.Fatal("repo_schedules-only patch was not recognized as runtime-only")
	}

	got := patch.Apply(Settings{})
	if len(got.RepoSchedules) != 1 || got.RepoSchedules[0].Namespace != "*" {
		t.Fatalf("repo_schedules patch result = %#v", got.RepoSchedules)
	}
}

func TestPatchApplyExplicitFalse(t *testing.T) {
	t.Parallel()

	var patch Patch
	if err := json.Unmarshal([]byte(`{"code_rag_enabled":false}`), &patch); err != nil {
		t.Fatal(err)
	}

	got := patch.Apply(Settings{CodeRAGEnabled: true})
	if got.CodeRAGEnabled {
		t.Error("explicit false was not applied")
	}
}

func TestPatchApplyMarkdownTargetSetting(t *testing.T) {
	t.Parallel()

	on := true
	got := (Patch{RAGKeepMarkdownTargets: &on}).Apply(Settings{})
	if !got.RAGKeepMarkdownTargets {
		t.Fatal("keep-markdown-targets setting was not enabled")
	}

	off := false
	got = (Patch{RAGKeepMarkdownTargets: &off}).Apply(got)
	if got.RAGKeepMarkdownTargets {
		t.Fatal("keep-markdown-targets setting was not disabled")
	}
}

func TestPatchApplyWebImageSettings(t *testing.T) {
	t.Parallel()

	var patch Patch
	if err := json.Unmarshal([]byte(`{
		"web_image_analysis_enabled":true,
		"web_image_model":"gpt-4.1-mini",
		"web_image_max_per_page":5,
		"web_image_max_bytes":2097152,
		"web_image_max_pixels":12000000,
		"web_image_allow_authenticated":true,
		"task_concurrency":2
	}`), &patch); err != nil {
		t.Fatal(err)
	}

	got := patch.Apply(Settings{})
	if !got.WebImageAnalysisEnabled || !got.WebImageAllowAuthenticated {
		t.Fatalf("true image settings were not applied: %#v", got)
	}
	if got.WebImageModel != "gpt-4.1-mini" || got.WebImageMaxPerPage != 5 ||
		got.WebImageMaxBytes != 2<<20 || got.WebImageMaxPixels != 12_000_000 {
		t.Fatalf("image settings were not applied: %#v", got)
	}
	if patch.RuntimeOnly() {
		t.Fatal("mixed runtime/image-analysis patch incorrectly recognized as runtime-only")
	}

	patch = Patch{}
	if err := json.Unmarshal([]byte(`{
		"web_image_analysis_enabled":false,
		"web_image_allow_authenticated":false
	}`), &patch); err != nil {
		t.Fatal(err)
	}
	got = patch.Apply(got)
	if got.WebImageAnalysisEnabled || got.WebImageAllowAuthenticated {
		t.Fatalf("explicit false image settings were not applied: %#v", got)
	}
}

func TestWebImageDefaults(t *testing.T) {
	t.Parallel()

	fresh := Defaults()
	if fresh.WebImageAnalysisEnabled || fresh.WebImageAllowAuthenticated {
		t.Fatalf("image analysis or authenticated fetching enabled by default: %#v", fresh)
	}
	if fresh.WebImageMaxPerPage != 3 || fresh.WebImageMaxBytes != 4<<20 || fresh.WebImageMaxPixels != 16_000_000 {
		t.Fatalf("fresh image defaults = %#v", fresh)
	}

	// A migrated record has zero for fields that did not exist in its schema.
	migrated := Settings{}
	if migrated.EffectiveWebImageMaxPerPage() != 3 || migrated.EffectiveWebImageMaxBytes() != 4<<20 ||
		migrated.EffectiveWebImageMaxPixels() != 16_000_000 {
		t.Fatalf("migrated image defaults = %#v", migrated.Redact().Settings)
	}
	redacted := migrated.Redact()
	if redacted.WebImageMaxPerPage != 3 || redacted.WebImageMaxBytes != 4<<20 ||
		redacted.WebImageMaxPixels != 16_000_000 {
		t.Fatalf("redacted image defaults = %#v", redacted.Settings)
	}
}

func TestWebImageLimitsRejectUnsafeValues(t *testing.T) {
	t.Parallel()
	for _, settings := range []Settings{
		{WebImageMaxPerPage: -1},
		{WebImageMaxBytes: -1},
		{WebImageMaxPixels: -1},
		{WebImageMaxPerPage: config.MaxWebImageMaxPerPage + 1},
		{WebImageMaxBytes: config.MaxWebImageMaxBytes + 1},
		{WebImageMaxPixels: config.MaxWebImageMaxPixels + 1},
	} {
		if err := settings.validateWebImageLimits(); err == nil {
			t.Fatalf("unsafe image limits accepted: %#v", settings)
		}
	}
}

func TestObservabilityOnly(t *testing.T) {
	on := true
	host := "https://cloud.langfuse.com"
	model := "gpt-4o"

	t.Run("langfuse fields alone", func(t *testing.T) {
		p := Patch{LangfuseEnabled: &on, LangfuseHost: &host}
		if !p.ObservabilityOnly() {
			t.Fatal("a langfuse-only patch must be recognised as observability-only")
		}
		// It must NOT take the runtime-only shortcut: the tracer is attached to
		// the LLM/embedder clients, so the bundle has to be rebuilt.
		if p.RuntimeOnly() {
			t.Fatal("a langfuse patch must not skip the client rebuild")
		}
	})

	t.Run("mixed with a model change", func(t *testing.T) {
		p := Patch{LangfuseEnabled: &on, LLMModel: &model}
		if p.ObservabilityOnly() {
			t.Fatal("a patch that also changes the model must trigger a reindex")
		}
	})

	t.Run("no langfuse field", func(t *testing.T) {
		p := Patch{LLMModel: &model}
		if p.ObservabilityOnly() {
			t.Fatal("a patch with no langfuse field is not observability-only")
		}
	})

	t.Run("empty patch", func(t *testing.T) {
		if (Patch{}).ObservabilityOnly() {
			t.Fatal("an empty patch is not observability-only")
		}
	})
}

func TestLangfuseSecretIsWriteOnly(t *testing.T) {
	s := Settings{LangfuseSecretKey: "sk-lf-secret", LangfusePublicKey: "pk-lf-public"}

	r := s.Redact()

	if r.Settings.LangfuseSecretKey != "" {
		t.Fatal("redacted view still carries the secret")
	}
	if !r.LangfuseSecretKeySet {
		t.Fatal("langfuse_secret_key_set should report the secret is present")
	}
	// The public key is not a secret and must survive, or the UI cannot show it.
	if r.Settings.LangfusePublicKey != "pk-lf-public" {
		t.Fatal("public key must not be redacted")
	}
}

func TestLangfusePatchApply(t *testing.T) {
	base := Settings{
		LangfuseHost:      "https://old.example",
		LangfuseCapture:   "full",
		LangfuseTraceDocs: true,
	}

	host := "https://new.example"
	off := false
	capture := "truncated"

	got := Patch{
		LangfuseHost:      &host,
		LangfuseCapture:   &capture,
		LangfuseTraceDocs: &off,
	}.Apply(base)

	if got.LangfuseHost != host {
		t.Errorf("host = %q want %q", got.LangfuseHost, host)
	}
	if got.LangfuseCapture != capture {
		t.Errorf("capture = %q want %q", got.LangfuseCapture, capture)
	}
	// An explicit false must land, which is the whole reason Patch uses
	// pointers rather than zero values.
	if got.LangfuseTraceDocs {
		t.Error("explicit false did not clear trace_docs")
	}
}

// A Go nil slice marshals to null. Clients index these lists directly, so the
// redacted view - the single funnel for every GET and PUT response - must
// always present them as arrays.
func TestRedactNeverReturnsNullLists(t *testing.T) {
	// The zero value is what a fresh install looks like: every list nil.
	body, err := json.Marshal(Settings{}.Redact())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	lists := []string{
		"repo_schedules",
		"docs_include",
		"docs_include_extra",
		"docs_exclude",
		"rag_lexical_stop_words",
		"code_rag_include",
		"code_rag_include_extra",
		"code_rag_exclude",
	}

	for _, key := range lists {
		raw, ok := decoded[key]
		if !ok {
			t.Errorf("%s missing from the response", key)

			continue
		}

		if string(raw) == "null" {
			t.Errorf("%s serialized as null; clients index it directly", key)
		}
	}
}

// Nested spec lists are covered too, and normalizing them must not write
// through into the caller's record.
func TestRedactDoesNotMutateSource(t *testing.T) {
	src := Settings{RepoSchedules: []RepoSchedule{{Namespace: "a", Specs: nil}}}

	r := src.Redact()

	if r.RepoSchedules[0].Specs == nil {
		t.Error("nested specs were left nil in the redacted view")
	}

	if src.RepoSchedules[0].Specs != nil {
		t.Error("Redact wrote through into the caller's schedules")
	}
}

// Normalizing must not change what the settings mean: an empty list and a nil
// list are both "nothing configured".
func TestRedactNormalizationPreservesSemantics(t *testing.T) {
	r := Settings{GitPollInterval: time.Hour}.Redact()

	if len(r.RepoSchedules) != 0 {
		t.Fatalf("expected no schedules, got %d", len(r.RepoSchedules))
	}

	// EffectiveSchedules falls back to the fixed interval when no schedule is
	// configured; an empty slice must behave exactly as nil did.
	if got := len(r.Settings.EffectiveSchedules()); got != len(Settings{GitPollInterval: time.Hour}.EffectiveSchedules()) {
		t.Errorf("empty schedules changed EffectiveSchedules (%d entries)", got)
	}
}
