package rag

import (
	"strings"
	"testing"
	"unicode/utf8"
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

// TestChunkWindowKeepsRunesWhole pins the oversized-section window against
// non-ASCII text. size and overlap are byte counts and the only cut
// adjustment is a newline lookup that gives up in the first half of the
// window, so a window edge used to land inside a multi-byte rune. The half
// rune is embedded and served verbatim as a search_docs excerpt, where it is
// indistinguishable from corruption in the source document.
func TestChunkWindowKeepsRunesWhole(t *testing.T) {
	cases := []struct {
		name string
		body string
		rune string
	}{
		// No newline at all, and 100 % 3 != 0, so every window edge falls
		// inside a rune with no line boundary to fall back to.
		{name: "3-byte runes", body: strings.Repeat("あ", 200), rune: "あ"},
		// The only newline sits in the first half of the window, where the
		// nl > step/2 guard rejects it, so the byte offset decides the cut.
		{name: "4-byte runes after heading", body: "# T\n" + strings.Repeat("🚀", 200), rune: "🚀"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := chunk(tc.body, 100, 20)
			if len(chunks) < 2 {
				t.Fatalf("expected windowed chunks, got %d", len(chunks))
			}

			for i, c := range chunks {
				if !utf8.ValidString(c) {
					t.Fatalf("chunk %d is not valid UTF-8: %q", i, c)
				}

				if rest := strings.Trim(c, tc.rune+"# T\n"); rest != "" {
					t.Fatalf("chunk %d holds unexpected bytes %q: %q", i, rest, c)
				}
			}

			// The windows must still cover the whole section: overlap only
			// duplicates text, so a shortfall means a cut dropped bytes.
			total := 0
			for _, c := range chunks {
				total += utf8.RuneCountInString(c)
			}

			if want := utf8.RuneCountInString(strings.TrimSpace(tc.body)); total < want {
				t.Fatalf("chunks hold %d runes, section has %d", total, want)
			}
		})
	}
}

// TestChunkWindowNarrowerThanRune pins the degenerate configured chunk size:
// when the window is smaller than the rune it starts on, backing the cut up
// to a rune boundary would leave an empty window and the loop would never
// advance past it.
func TestChunkWindowNarrowerThanRune(t *testing.T) {
	chunks := window(strings.Repeat("あ", 4), 1, 0)

	if len(chunks) != 4 {
		t.Fatalf("got %d chunks want 4: %q", len(chunks), chunks)
	}

	for i, c := range chunks {
		if c != "あ" {
			t.Fatalf("chunk %d = %q", i, c)
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
