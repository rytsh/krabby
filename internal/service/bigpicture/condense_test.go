package bigpicture

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rytsh/krabby/internal/service/llm"
)

type completerFunc func(context.Context, []llm.Message) (string, error)

func (f completerFunc) Complete(ctx context.Context, m []llm.Message) (string, error) {
	return f(ctx, m)
}

type memoryNotes struct {
	mu    sync.Mutex
	notes map[string]string
}

func (c *memoryNotes) Note(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	text, ok := c.notes[key]
	return text, ok
}

func (c *memoryNotes) SaveNote(key, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notes[key] = text
	return nil
}

func TestCondenseManySources(t *testing.T) {
	selector := Source{Kind: "repo_pattern", Ref: "github.com/acme/**"}
	p := &Picture{Name: "acme", Title: "Acme", Prompt: "Explain Kafka flows", Sources: []Source{selector, {Kind: "web", Ref: "jira"}}}
	var research Research
	add := func(source Source, locator string, size int) {
		research.Items = append(research.Items, ResearchItem{ID: fmt.Sprintf("e%d", len(research.Items)+1), Evidence: Evidence{Source: source, Locator: locator}, Content: strings.Repeat("x", size)})
	}
	// 200 repositories: half have an integration profile.
	for i := range 200 {
		repo := fmt.Sprintf("github.com/acme/svc-%03d", i)
		if i%2 == 0 {
			add(selector, repo+":"+ProfileLocator, 2<<10)
		}
		add(selector, repo+":README.md", 12<<10)
	}
	add(Source{Kind: "web", Ref: "jira"}, "ACME-1.md", 20<<10)

	var calls atomic.Int32
	model := completerFunc(func(_ context.Context, m []llm.Message) (string, error) {
		calls.Add(1)
		if strings.HasPrefix(m[0].Content, "You merge") {
			return strings.Repeat("m", 4<<10), nil
		}
		return "## Publishes\n- `order-created`", nil
	})
	cache := &memoryNotes{notes: map[string]string{}}

	out, used, err := Condense(context.Background(), model, cache, p, research)
	if err != nil {
		t.Fatal(err)
	}
	if researchBytes(out.Items) > DirectBudget {
		t.Fatalf("condensed research %d exceeds budget", researchBytes(out.Items))
	}
	// Repositories with a profile and small units need no model call.
	if notes := calls.Load(); notes < 101 || notes > 140 {
		t.Fatalf("model calls = %d, want ~100 notes + 1 jira + few merges", notes)
	}
	if len(used) == 0 {
		t.Fatal("no cache keys reported")
	}

	// Every source used for synthesis stays citable through the condensed
	// notes. Raw files of a repository summarized by its integration profile
	// were not sent to the model, so they are listed in research.md only.
	covered := map[Evidence]bool{}
	for _, item := range out.Items {
		if item.ID == "" {
			t.Fatal("condensed item without id")
		}
		for _, e := range item.citations() {
			covered[e] = true
		}
	}
	for i, item := range research.Items {
		replacedByProfile := strings.HasSuffix(item.Evidence.Locator, ":README.md") && i > 0 && strings.HasSuffix(research.Items[i-1].Evidence.Locator, ProfileLocator)
		if covered[item.Evidence] == replacedByProfile {
			t.Fatalf("evidence %v: covered=%t, replaced by profile=%t", item.Evidence, covered[item.Evidence], replacedByProfile)
		}
	}

	// A second run with the same research is served from the cache.
	before := calls.Load()
	if _, _, err := Condense(context.Background(), model, cache, p, research); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before {
		t.Fatalf("cached run made %d model calls", calls.Load()-before)
	}

	// Small research passes through unchanged.
	small := Research{Items: research.Items[:3]}
	same, _, err := Condense(context.Background(), model, cache, p, small)
	if err != nil || len(same.Items) != 3 || same.Items[0].Content != small.Items[0].Content {
		t.Fatalf("small research changed: %v", err)
	}
}
