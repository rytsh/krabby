package settings

import (
	"encoding/json"
	"testing"
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
