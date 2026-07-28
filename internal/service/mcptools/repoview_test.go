package mcptools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/registry"
)

// A page of repositories is a discovery response, so it must stay small. The
// documentation prompts are override inputs that can run to kilobytes each;
// leaking them into every listing would let 20 repos dominate the response.
func TestRepoViewStripsPrompts(t *testing.T) {
	repo := &registry.Repo{
		ID: "acme/deploy",
		Overrides: registry.Overrides{
			IncludeExtra:    []string{"**/*.yaml"},
			DocsPrompt:      "SECRET-LONG-PROMPT",
			DocsPromptExtra: "SECRET-LONG-EXTRA",
		},
	}

	raw, err := json.Marshal(trimRepoForView(repo))
	if err != nil {
		t.Fatal(err)
	}

	for _, leak := range []string{"SECRET-LONG-PROMPT", "SECRET-LONG-EXTRA"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("prompt leaked into the repo view: %s", raw)
		}
	}

	// The glob lists stay: they are short and tell the model what is indexed.
	if !strings.Contains(string(raw), "**/*.yaml") {
		t.Fatalf("include_extra should survive: %s", raw)
	}

	// The caller's record must not be mutated by rendering a view of it.
	if repo.Overrides.DocsPrompt != "SECRET-LONG-PROMPT" {
		t.Fatal("viewRepo mutated the stored record")
	}
}
