package settings

import (
	"encoding/json"
	"testing"
	"time"
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
