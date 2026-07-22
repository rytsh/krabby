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
