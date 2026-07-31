package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/service/websource"
)

func TestWebImageSettingsChanged(t *testing.T) {
	t.Parallel()
	base := settings.Settings{WebImageMaxPerPage: 3, WebImageMaxBytes: 4 << 20, WebImageMaxPixels: 16_000_000}
	unrelated := base
	unrelated.RAGTopK = 99
	if webImageSettingsChanged(base, unrelated) {
		t.Fatal("unrelated RAG setting triggered image refresh")
	}
	changed := base
	changed.WebImageModel = "vision-v2"
	if !webImageSettingsChanged(base, changed) {
		t.Fatal("vision model change did not trigger image refresh")
	}
	fallback := base
	fallback.LLMModel = "main-v2"
	if !webImageSettingsChanged(base, fallback) {
		t.Fatal("main model change did not refresh blank vision-model fallback")
	}
	provider := base
	provider.LLMBaseURL = "https://new-provider.example/v1"
	if !webImageSettingsChanged(base, provider) {
		t.Fatal("provider change did not trigger image refresh")
	}
}

type visionTestFetcher struct {
	*fakeReconcileFetcher
	image   []byte
	fetches atomic.Int32
}

func (f *visionTestFetcher) FetchImage(context.Context, *websource.Collection, string, string, int64, bool) (websource.ImageContent, error) {
	f.fetches.Add(1)
	return websource.ImageContent{Data: append([]byte(nil), f.image...), MediaType: "image/png"}, nil
}

func testPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, c)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWebImageAnalysisIsCachedAndPersistedBeforeIndexing(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"type":"image_url"`)) || !bytes.Contains(body, []byte("data:image/png;base64,")) {
			t.Errorf("multimodal request = %s", body)
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "vision-test",
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": "The diagram connects alpha to beta."},
			}},
		})
	}))
	defer visionServer.Close()

	vision, err := llm.New(config.LLM{BaseURL: visionServer.URL, Model: "vision-test"})
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &visionTestFetcher{
		fakeReconcileFetcher: &fakeReconcileFetcher{pages: []websource.RemotePage{{
			Slug: "architecture", Title: "Architecture", URL: "https://docs.example/page",
			Markdown: "alpha documentation\n\n![Flow](https://docs.example/flow.png)",
		}}},
		image: testPNG(t, color.RGBA{R: 255, A: 255}),
	}
	m, store := newReconcileManager(t, fetcher)
	m.docs.vision = vision
	m.docs.imageCfg = config.WebImage{
		AnalysisEnabled: true, Model: "vision-test", MaxPerPage: 3, MaxBytes: 1 << 20, MaxPixels: 100,
	}
	if err := store.UpsertCollection(ctx, &websource.Collection{
		Name: "images", Type: "fake", AnalyzeImages: true, Status: websource.StatusPending,
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.RefreshWebSource(ctx, "images"); err != nil {
		t.Fatal(err)
	}
	path, err := websource.PageFile(m.sourcesDir("images"), "architecture")
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markdown), "**Image analysis:** The diagram connects alpha to beta.") {
		t.Fatalf("markdown = %q", markdown)
	}
	if strings.Contains(string(markdown), "base64") {
		t.Fatalf("image bytes leaked into markdown: %q", markdown)
	}

	if err := m.refreshWebSource(ctx, "images", true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || fetcher.fetches.Load() != 2 {
		t.Fatalf("after cache hit: vision calls=%d image fetches=%d", calls.Load(), fetcher.fetches.Load())
	}

	fetcher.image = testPNG(t, color.RGBA{B: 255, A: 255})
	if err := m.refreshWebSource(ctx, "images", true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("changed image did not invalidate cache; calls=%d", calls.Load())
	}
}
