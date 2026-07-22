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

func TestSearchCodeArgsMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    string
		want    string
		wantErr bool
	}{
		{name: "default", want: "normal"},
		{name: "normal", mode: "normal", want: "normal"},
		{name: "semantic", mode: "semantic", want: "semantic"},
		{name: "invalid", mode: "hybrid", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (searchCodeArgs{Mode: tt.mode}).searchMode()
			if (err != nil) != tt.wantErr {
				t.Fatalf("searchMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("searchMode() = %q, want %q", got, tt.want)
			}
		})
	}
}
