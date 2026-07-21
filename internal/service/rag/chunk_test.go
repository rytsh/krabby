package rag

import (
	"strings"
	"testing"
)

func TestChunkSplitsOnHeadings(t *testing.T) {
	md := "# Title\nintro text\n\n## Section A\naaaa\n\n## Section B\nbbbb\n"

	chunks := chunk(md, 30, 5)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d: %q", len(chunks), chunks)
	}

	for _, c := range chunks {
		if len(c) > 30 {
			t.Fatalf("chunk exceeds size cap: %d chars", len(c))
		}
	}
}

func TestChunkPacksSmallSections(t *testing.T) {
	md := "## A\na\n\n## B\nb\n"

	chunks := chunk(md, 1200, 200)
	if len(chunks) != 1 {
		t.Fatalf("small sections should pack into one chunk, got %d", len(chunks))
	}

	if !strings.Contains(chunks[0], "## A") || !strings.Contains(chunks[0], "## B") {
		t.Fatalf("packed chunk missing sections: %q", chunks[0])
	}
}

func TestChunkWindowsOversizedSection(t *testing.T) {
	body := strings.Repeat("word ", 200) // ~1000 chars, no headings

	chunks := chunk(body, 300, 50)
	if len(chunks) < 3 {
		t.Fatalf("expected windowed chunks, got %d", len(chunks))
	}

	for _, c := range chunks {
		if len(c) > 300 {
			t.Fatalf("window exceeds size cap: %d chars", len(c))
		}
	}
}

func TestChunkIgnoresHeadingsInCodeFences(t *testing.T) {
	md := "## Real\ntext\n```\n# not a heading\n```\nmore\n"

	chunks := chunk(md, 1200, 200)
	if len(chunks) != 1 {
		t.Fatalf("code-fence comment must not split, got %d chunks", len(chunks))
	}
}

func TestFirstHeading(t *testing.T) {
	if got := firstHeading("intro\n## Hello World\ntext"); got != "Hello World" {
		t.Fatalf("firstHeading = %q", got)
	}

	if got := firstHeading("no headings here"); got != "" {
		t.Fatalf("firstHeading = %q want empty", got)
	}
}
