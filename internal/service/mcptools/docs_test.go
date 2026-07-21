package mcptools

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/settings"
)

func TestSetDocsConfigArgsMergePresence(t *testing.T) {
	t.Parallel()

	base := settings.Settings{
		DocsEnabled:  true,
		EmbedBaseURL: "https://embed.example/v1",
		RAGEnabled:   true,
		RAGTopK:      20,
	}
	raw := json.RawMessage(`{"code_rag_enabled":true,"code_rag_top_k":5,"code_embed_timeout":"45s"}`)
	args := setDocsConfigArgs{
		CodeRAGEnabled:   true,
		CodeRAGTopK:      5,
		CodeEmbedTimeout: "45s",
	}

	got, err := args.merge(base, raw)
	if err != nil {
		t.Fatal(err)
	}

	if !got.CodeRAGEnabled || got.CodeRAGTopK != 5 || got.CodeEmbedTimeout != 45*time.Second {
		t.Errorf("code fields not merged: %#v", got)
	}

	if !got.DocsEnabled || !got.RAGEnabled || got.RAGTopK != 20 || got.EmbedBaseURL != base.EmbedBaseURL {
		t.Errorf("omitted fields changed: %#v", got)
	}
}
