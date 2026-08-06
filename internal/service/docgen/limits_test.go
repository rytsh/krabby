package docgen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/llm"
)

// bodyRecordingLLM captures the user message of every chat call so a test can
// assert how much source text actually reached the model.
func bodyRecordingLLM(t *testing.T, seen *[]string, mu *sync.Mutex) *llm.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "user" {
				*seen = append(*seen, m.Content)
			}
		}
		mu.Unlock()

		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"## big.go\ns"}}]}`))
	}))
	t.Cleanup(srv.Close)

	c, err := llm.New(config.LLM{BaseURL: srv.URL, Model: "test"})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	return c
}

// bigSource returns a source file of at least n bytes whose tail is uniquely
// identifiable, so a test can tell truncation from full delivery.
func bigSource(n int) (body, marker string) {
	marker = "func TailMarkerXYZ() {}"

	var b strings.Builder
	b.WriteString("package big\n")
	for b.Len() < n {
		b.WriteString("func Filler() { _ = 1 } // padding padding padding padding\n")
	}
	b.WriteString(marker + "\n")

	return b.String(), marker
}

func summaryCalls(seen []string) []string {
	var out []string
	for _, s := range seen {
		if strings.Contains(s, "===== FILE:") {
			out = append(out, s)
		}
	}

	return out
}

// The default budget is the reason a repository whose substance lives in a few
// very large files gets a thin, hedging documentation.md: the model is shown a
// prefix and never sees the rest.
func TestSummaryTruncatesAtDefaultSourceBudget(t *testing.T) {
	clone := t.TempDir()
	body, marker := bigSource(config.DefaultDocsMaxSourceBytes * 2)
	writeSrc(t, clone, "big.go", body)

	var (
		mu   sync.Mutex
		seen []string
	)
	gen := New(config.Docs{}, bodyRecordingLLM(t, &seen, &mu), nil, nil, nil)

	if _, err := gen.Generate(context.Background(), "o/r", clone,
		filepath.Join(clone, "krabby-docs"), config.DocsOverride{}, false); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	calls := summaryCalls(seen)
	if len(calls) != 1 {
		t.Fatalf("summary calls = %d, want 1", len(calls))
	}
	if strings.Contains(calls[0], marker) {
		t.Error("the file tail reached the model at the default budget; the cap is not being applied")
	}
}

// Raising the install-wide budget must actually widen what the model reads —
// this is the knob, and nothing downstream may re-clamp it.
func TestGlobalSourceBudgetRaisesWhatReachesTheModel(t *testing.T) {
	clone := t.TempDir()
	size := config.DefaultDocsMaxSourceBytes * 2
	body, marker := bigSource(size)
	writeSrc(t, clone, "big.go", body)

	var (
		mu   sync.Mutex
		seen []string
	)
	cfg := config.Docs{Limits: config.DocsLimits{
		MaxSourceBytes: size * 4,
		MaxGroupBytes:  size * 4,
	}}
	gen := New(cfg, bodyRecordingLLM(t, &seen, &mu), nil, nil, nil)

	if _, err := gen.Generate(context.Background(), "o/r", clone,
		filepath.Join(clone, "krabby-docs"), config.DocsOverride{}, false); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	calls := summaryCalls(seen)
	if len(calls) != 1 {
		t.Fatalf("summary calls = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0], marker) {
		t.Error("the file tail still did not reach the model after raising the budget")
	}
}

// The per-repository override is what makes one deployment repo of huge YAML
// workable without loosening every other repository's budget.
func TestRepoOverrideRaisesSourceBudget(t *testing.T) {
	clone := t.TempDir()
	size := config.DefaultDocsMaxSourceBytes * 2
	body, marker := bigSource(size)
	writeSrc(t, clone, "big.go", body)

	var (
		mu   sync.Mutex
		seen []string
	)
	gen := New(config.Docs{}, bodyRecordingLLM(t, &seen, &mu), nil, nil, nil)

	over := config.DocsOverride{Limits: config.DocsLimits{
		MaxSourceBytes: size * 4,
		MaxGroupBytes:  size * 4,
	}}
	if _, err := gen.Generate(context.Background(), "o/r", clone,
		filepath.Join(clone, "krabby-docs"), over, false); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	calls := summaryCalls(seen)
	if len(calls) != 1 {
		t.Fatalf("summary calls = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0], marker) {
		t.Error("the repo override did not widen the source budget")
	}
}

// A generous per-file budget is still bounded by the group budget, which is
// split across the group's files. Raising only MaxSourceBytes is therefore not
// enough when several large files share one call, and the tool schema says so.
func TestGroupBudgetStillBindsWhenSourceBudgetRaised(t *testing.T) {
	clone := t.TempDir()
	size := 40 * 1024
	body, marker := bigSource(size)
	writeSrc(t, clone, "a.go", body)
	writeSrc(t, clone, "b.go", body)
	writeSrc(t, clone, "c.go", body)
	writeSrc(t, clone, "d.go", body)
	// The group budget only binds when several files share one call, which is
	// what a graph community does.
	writeGraph(t, clone, map[string]int{"a.go": 0, "b.go": 0, "c.go": 0, "d.go": 0})

	var (
		mu   sync.Mutex
		seen []string
	)
	// Ample per-file budget, deliberately tight group budget: 4 files share
	// 60 KiB, so each gets 15 KiB of a 40 KiB file.
	cfg := config.Docs{Limits: config.DocsLimits{
		MaxSourceBytes: 1 << 20,
		MaxGroupBytes:  60 * 1024,
	}}
	gen := New(cfg, bodyRecordingLLM(t, &seen, &mu), nil, graphquery.NewEngine(0), nil)

	if _, err := gen.Generate(context.Background(), "o/r", clone,
		filepath.Join(clone, "krabby-docs"), config.DocsOverride{}, false); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if got := len(summaryCalls(seen)); got != 1 {
		t.Fatalf("summary calls = %d, want the four files grouped into 1", got)
	}

	for _, call := range summaryCalls(seen) {
		if strings.Contains(call, marker) {
			t.Fatal("group budget was not applied; a whole file reached the model")
		}
	}
}

// Raising the budget must re-summarize the files that were truncated before:
// the cached summary is keyed on the content hash, and the content is the
// capped content, so a wider cap is a different input.
func TestRaisingSourceBudgetInvalidatesTruncatedSummaries(t *testing.T) {
	clone := t.TempDir()
	size := config.DefaultDocsMaxSourceBytes * 2
	body, _ := bigSource(size)
	writeSrc(t, clone, "big.go", body)

	docsDir := filepath.Join(clone, "krabby-docs")

	var (
		mu   sync.Mutex
		seen []string
	)
	gen := New(config.Docs{}, bodyRecordingLLM(t, &seen, &mu), nil, nil, nil)
	if _, err := gen.Generate(context.Background(), "o/r", clone, docsDir, config.DocsOverride{}, false); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	before := len(summaryCalls(seen))

	wide := config.DocsOverride{Limits: config.DocsLimits{
		MaxSourceBytes: size * 4,
		MaxGroupBytes:  size * 4,
	}}
	if _, err := gen.Generate(context.Background(), "o/r", clone, docsDir, wide, false); err != nil {
		t.Fatalf("Generate (wide): %v", err)
	}

	if got := len(summaryCalls(seen)); got <= before {
		t.Error("a wider budget reused the truncated summary; it must be regenerated")
	}
}
