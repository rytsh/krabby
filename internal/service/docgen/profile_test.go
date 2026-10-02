package docgen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/llm"
)

// profileLLM answers the integration-profile prompt with a recognizable body,
// fails it while failProfile is set, and counts profile calls.
func profileLLM(t *testing.T, profileCalls *int32, failProfile *atomic.Bool) *llm.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Content == ProfilePrompt {
			atomic.AddInt32(profileCalls, 1)
			if failProfile.Load() {
				http.Error(w, "boom", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"## Role\nOrders service.\n\n## Publishes\n- Kafka ` + "`order-created`" + `"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"# Doc\n\ngenerated"}}]}`))
	}))
	t.Cleanup(srv.Close)

	c, err := llm.New(config.LLM{BaseURL: srv.URL, Model: "test"})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	return c
}

func TestIntegrationProfile(t *testing.T) {
	clone := t.TempDir()
	writeSrc(t, clone, "main.go", "package main\nfunc main() {}\n")

	var calls int32
	var fail atomic.Bool
	gen := New(config.Docs{}, profileLLM(t, &calls, &fail), nil, nil, nil)
	docsDir := filepath.Join(clone, "krabby-docs")

	man, err := gen.Generate(context.Background(), "acme/orders", clone, docsDir, config.DocsOverride{}, false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(man.Docs) != 2 || man.Docs[1].Path != ProfileName || !man.ChangedDocs {
		t.Fatalf("docs = %+v, want documentation.md and %s", man.Docs, ProfileName)
	}
	body, err := os.ReadFile(filepath.Join(docsDir, ProfileName))
	if err != nil || !strings.Contains(string(body), "# acme/orders integration profile") || !strings.Contains(string(body), "order-created") {
		t.Fatalf("profile = %q, %v", body, err)
	}

	// Unchanged sources reuse the profile without a model call.
	if _, err := gen.Generate(context.Background(), "acme/orders", clone, docsDir, config.DocsOverride{}, false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("profile calls = %d, want 1 (reused when unchanged)", calls)
	}

	// A failed profile keeps the previous one and does not fail the build.
	writeSrc(t, clone, "main.go", "package main\nfunc main() { run() }\n")
	fail.Store(true)
	man, err = gen.Generate(context.Background(), "acme/orders", clone, docsDir, config.DocsOverride{}, false)
	if err != nil {
		t.Fatalf("profile failure failed the build: %v", err)
	}
	if len(man.Docs) != 2 || man.ProfileHash != "" {
		t.Fatalf("previous profile not kept for retry: %+v hash=%q", man.Docs, man.ProfileHash)
	}

	// The next build retries even though the sources did not change again.
	fail.Store(false)
	if _, err := gen.Generate(context.Background(), "acme/orders", clone, docsDir, config.DocsOverride{}, false); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("profile calls = %d, want a retry after the failure", calls)
	}
}
