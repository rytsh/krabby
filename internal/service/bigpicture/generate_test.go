package bigpicture

import (
	"context"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/llm"
)

type completionFunc func(context.Context, []llm.Message) (string, error)

func (f completionFunc) Complete(ctx context.Context, messages []llm.Message) (string, error) {
	return f(ctx, messages)
}

func TestGeneratePinsEvidenceAndPublication(t *testing.T) {
	p := &Picture{Name: "commerce", StorageID: "instance", Version: 7, CurrentRevision: "old", Title: "Commerce", Prompt: "Explain Kafka flows", Sources: []Source{{Kind: "repo", Ref: "example/team/service"}}}
	research := Research{Items: []ResearchItem{{ID: "e1", Evidence: Evidence{Source: p.Sources[0], Locator: "deploy/values.yaml", Revision: "abc"}, Content: "consumer: payment", Truncated: true}}, Notes: []string{"Partial collection"}, Previous: []Document{{Path: "old.md", Title: "Old", Markdown: "Old structure"}}}
	client := completionFunc(func(_ context.Context, messages []llm.Message) (string, error) {
		if len(messages) != 2 || !strings.Contains(messages[0].Content, "untrusted data") || !strings.Contains(messages[1].Content, "Old structure") {
			t.Fatal("research context or injection boundaries missing")
		}
		return `{"overview":"overview.md","documents":[{"path":"overview.md","title":"Overview","markdown":"# Overview\n[Coverage](research.md)","evidence_ids":["e1","e1"]}]}`, nil
	})
	pub, err := Generate(context.Background(), client, p, research)
	if err != nil {
		t.Fatal(err)
	}
	if pub.ExpectedVersion != 7 || pub.ExpectedRevision != "old" || pub.ExpectedInstance != "instance" || len(pub.Documents) != 2 {
		t.Fatalf("publication: %+v", pub)
	}
	if len(pub.Documents[0].Evidence) != 1 || pub.Documents[0].Evidence[0].Locator != "deploy/values.yaml" {
		t.Fatal("model citation did not resolve to collected evidence")
	}
	if !strings.Contains(pub.Documents[1].Markdown, "truncated: true") {
		t.Fatal("coverage truncation omitted")
	}
}

func TestGenerateRejectsUnsupportedOutput(t *testing.T) {
	p := &Picture{Version: 1, Sources: []Source{{Kind: "repo", Ref: "repo"}}}
	research := Research{Items: []ResearchItem{{ID: "e1", Evidence: Evidence{Source: p.Sources[0], Locator: "README.md"}, Content: "Architecture"}}}
	for _, text := range []string{
		`not JSON`,
		`{"overview":"overview.md","documents":[{"path":"overview.md","title":"Overview","markdown":"Text","evidence_ids":["invented"]}]}`,
		`{"overview":"overview.md","documents":[{"path":"overview.md","title":"Overview","markdown":"Text","evidence_ids":[]}]}`,
		`{"overview":"../escape.md","documents":[{"path":"../escape.md","title":"Overview","markdown":"Text","evidence_ids":["e1"]}]}`,
		`{"overview":"overview.md","documents":[],"expected_version":9}`,
		`{"overview":"overview.md","documents":[]} {}`,
	} {
		if _, err := Generate(context.Background(), completionFunc(func(context.Context, []llm.Message) (string, error) { return text, nil }), p, research); err == nil {
			t.Fatalf("accepted: %s", text)
		}
	}
	called := false
	if _, err := Generate(context.Background(), completionFunc(func(context.Context, []llm.Message) (string, error) { called = true; return "", nil }), p, Research{}); err == nil || called {
		t.Fatal("empty research reached model")
	}
}
