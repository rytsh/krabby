package mcptools

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/manager"
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
	raw := json.RawMessage(`{"code_rag_enabled":true,"code_rag_top_k":5,"code_embed_timeout":"45s","web_image_analysis_enabled":true,"web_image_model":"vision-fast","rag_keep_markdown_targets":true}`)
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
	if !got.WebImageAnalysisEnabled || got.WebImageModel != "vision-fast" || !got.RAGKeepMarkdownTargets {
		t.Errorf("vision/projection fields not merged: %#v", got)
	}
}

func TestSourceArgsExposeScopeAndImageOptIn(t *testing.T) {
	t.Parallel()
	col, err := (addSourceArgs{Name: "Team-Wiki", Type: "pages", AnalyzeImages: true}).collection()
	if err != nil {
		t.Fatal(err)
	}
	if !col.AnalyzeImages {
		t.Fatal("add_source dropped analyze_images")
	}
	summary := summarizeSource(col)
	if summary.ScopeKey != "web:team-wiki" || summary.Name != "team-wiki" || !summary.AnalyzeImages {
		t.Fatalf("source summary = %#v", summary)
	}

	update, err := (updateSourceArgs{AnalyzeImages: false}).update(json.RawMessage(`{"analyze_images":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if !update.AnalyzeImages.Valid || update.AnalyzeImages.ValueOrZero() {
		t.Fatalf("explicit false analyze_images = %#v", update.AnalyzeImages)
	}
}

func TestImportSourcePagesArgs(t *testing.T) {
	t.Parallel()
	args := importSourcePagesArgs{Name: "notes", Pages: []sourcePageInput{{
		Title: "Runbook", ContentType: "text/markdown", Content: "Restart it.", UpdatedAt: "2026-07-31T12:00:00Z",
	}}}
	imports, err := args.imports()
	if err != nil {
		t.Fatal(err)
	}
	if len(imports) != 1 || imports[0].Title != "Runbook" || imports[0].UpdatedAt.IsZero() {
		t.Fatalf("imports = %#v", imports)
	}
	args.Pages[0].UpdatedAt = "not-a-time"
	if _, err := args.imports(); err == nil {
		t.Fatal("invalid updated_at was accepted")
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

func TestSearchDocsArgsMode(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		mode    string
		want    string
		wantErr bool
	}{
		// An omitted mode stays empty here: the manager resolves the default
		// against what the installation has configured.
		{want: ""},
		{mode: manager.DocsSearchHybrid, want: manager.DocsSearchHybrid},
		{mode: manager.DocsSearchSemantic, want: manager.DocsSearchSemantic},
		{mode: manager.DocsSearchLexical, want: manager.DocsSearchLexical},
		{mode: "normal", wantErr: true},
	} {
		got, err := (searchDocsArgs{Mode: tt.mode}).searchMode()
		if (err != nil) != tt.wantErr {
			t.Fatalf("searchMode(%q) error = %v, wantErr %v", tt.mode, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("searchMode(%q) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}
