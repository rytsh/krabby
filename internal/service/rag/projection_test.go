package rag

import (
	"strings"
	"testing"
)

func TestSearchableMarkdownDropsLinkAndImageDestinations(t *testing.T) {
	t.Parallel()

	input := `# Guide

Read the [authentication guide](https://docs.example.com/auth?token=noise) and
inspect ![request flow](data:image/png;base64,very-long-payload).
Nested destinations such as [API](https://example.com/v1/items(legacy)) work.
Bare endpoint text https://api.example.com/v1 remains useful, but
<https://tracking.example.com/click> does not.`

	got := searchableMarkdown(input, false)
	for _, want := range []string{"authentication guide", "request flow", "API", "https://api.example.com/v1"} {
		if !strings.Contains(got, want) {
			t.Errorf("projection missing %q: %q", want, got)
		}
	}
	for _, unwanted := range []string{"token=noise", "very-long-payload", "items(legacy)", "tracking.example.com"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("projection retained %q: %q", unwanted, got)
		}
	}

	if got := searchableMarkdown(input, true); got != input {
		t.Fatalf("keep-targets projection changed markdown: %q", got)
	}
}

func TestSearchableMarkdownHandlesReferencesHTMLAndCode(t *testing.T) {
	t.Parallel()
	input := "Use [the guide][private] and ![diagram][flow].\n" +
		"[private]:\n  https://secret.example/token\n  \"private title\"\n" +
		"[flow]: https://secret.example/flow.png\n" +
		"<a href=\"https://secret.example/raw\">visible HTML</a>\n" +
		"`[code](https://keep.example/inline)`\n" +
		"    [code](https://keep.example/indented)\n" +
		"\\[literal](https://keep.example/escaped)\n" +
		"```markdown\n[code](https://keep.example/fenced)\n```\n" +
		"<HTTPS://secret.example/autolink>\n"
	got := searchableMarkdown(input, false)
	for _, want := range []string{"the guide", "diagram", "visible HTML", "https://keep.example/inline", "https://keep.example/indented", "https://keep.example/escaped", "https://keep.example/fenced"} {
		if !strings.Contains(got, want) {
			t.Errorf("projection missing %q: %q", want, got)
		}
	}
	for _, unwanted := range []string{"secret.example", "[private]", "[flow]", "href="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("projection retained %q: %q", unwanted, got)
		}
	}
}

func TestWithSearchTitleAvoidsDuplicateHeading(t *testing.T) {
	t.Parallel()

	if got := withSearchTitle("# Guide\n\nBody", "Guide"); got != "# Guide\n\nBody" {
		t.Fatalf("matching heading changed: %q", got)
	}
	if got := withSearchTitle("## Details\n\nBody", "Guide"); !strings.HasPrefix(got, "# Guide\n\n## Details") {
		t.Fatalf("title was not prefixed: %q", got)
	}
}
