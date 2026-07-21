package coderag

import (
	"sort"
	"strconv"
	"strings"
)

// chunk is one embeddable slice of a source file with its line range.
type chunk struct {
	Text      string
	Symbol    string // leading symbol in the chunk ("" when unknown)
	StartLine int    // 1-based inclusive
	EndLine   int    // 1-based inclusive
}

// symbol is a graph node anchored to a line of a source file.
type symbol struct {
	Name string
	Line int // 1-based
}

// maxChunkChars hard-caps a single chunk so one pathological line (minified
// bundles, embedded blobs) cannot blow the embedder's context window.
func maxChunkChars(size int) int { return 2 * size }

// chunkFile splits a source file into chunks. When symbols (from the graphify
// graph) are available, chunk boundaries follow symbol boundaries: each chunk
// starts at a symbol and greedily absorbs following symbols up to size chars.
// Without symbols it falls back to line-aligned windows of ~size chars with
// ~overlap chars of overlap.
func chunkFile(content string, syms []symbol, size, overlap int) []chunk {
	if strings.TrimSpace(content) == "" {
		return nil
	}

	if size <= 0 {
		size = 3000
	}

	if overlap < 0 || overlap >= size {
		overlap = size / 3
	}

	lines := strings.Split(content, "\n")

	sections := sectionize(lines, syms)
	if len(sections) == 0 {
		return lineChunks(lines, 1, "", size, overlap)
	}

	var out []chunk

	// Greedily pack adjacent sections into chunks of up to size chars.
	i := 0
	for i < len(sections) {
		sec := sections[i]

		text := joinLines(lines, sec.start, sec.end)
		symName := sec.symbol

		// A single oversized section is split line-wise on its own.
		if len(text) > size {
			out = append(out, lineChunks(lines[sec.start-1:sec.end], sec.start, symName, size, overlap)...)
			i++

			continue
		}

		end := sec.end
		j := i + 1

		for j < len(sections) {
			next := joinLines(lines, sections[j].start, sections[j].end)
			if len(text)+len(next)+1 > size {
				break
			}

			text += "\n" + next
			if symName == "" {
				symName = sections[j].symbol
			}
			end = sections[j].end
			j++
		}

		out = append(out, chunk{
			Text:      capText(text, size),
			Symbol:    symName,
			StartLine: sec.start,
			EndLine:   end,
		})

		i = j
	}

	return out
}

// section is a run of lines belonging to one symbol (or the pre-symbol
// preamble: imports, package docs, constants before the first symbol).
type section struct {
	symbol string
	start  int // 1-based inclusive
	end    int // 1-based inclusive
}

// sectionize converts symbol anchors into contiguous line sections covering the
// whole file. Returns nil when there are no usable symbols.
func sectionize(lines []string, syms []symbol) []section {
	n := len(lines)
	sort.Slice(syms, func(i, j int) bool {
		if syms[i].Line == syms[j].Line {
			return syms[i].Name < syms[j].Name
		}

		return syms[i].Line < syms[j].Line
	})

	// Clamp, drop invalid, dedupe by line (keep first name seen).
	seen := map[int]string{}

	var starts []int

	for _, s := range syms {
		if s.Line < 1 || s.Line > n {
			continue
		}

		if _, ok := seen[s.Line]; !ok {
			seen[s.Line] = s.Name
			starts = append(starts, s.Line)
		}
	}

	if len(starts) == 0 {
		return nil
	}

	sort.Ints(starts)

	var out []section

	// Preamble before the first symbol.
	if starts[0] > 1 {
		out = append(out, section{symbol: "", start: 1, end: starts[0] - 1})
	}

	for i, st := range starts {
		end := n
		if i+1 < len(starts) {
			end = starts[i+1] - 1
		}

		out = append(out, section{symbol: seen[st], start: st, end: end})
	}

	return out
}

// lineChunks splits lines (whose first element is file line firstLine) into
// line-aligned windows of ~size chars with ~overlap chars of trailing overlap.
func lineChunks(lines []string, firstLine int, symName string, size, overlap int) []chunk {
	var out []chunk

	i := 0
	for i < len(lines) {
		var (
			b     strings.Builder
			taken int
		)

		for i+taken < len(lines) {
			ln := lines[i+taken]
			if taken > 0 && b.Len()+len(ln)+1 > size {
				break
			}

			if taken > 0 {
				b.WriteByte('\n')
			}

			b.WriteString(ln)
			taken++

			if b.Len() >= size {
				break
			}
		}

		if taken == 0 { // defensive; loop conditions guarantee progress
			break
		}

		text := b.String()
		if strings.TrimSpace(text) != "" {
			out = append(out, chunk{
				Text:      capText(text, size),
				Symbol:    symName,
				StartLine: firstLine + i,
				EndLine:   firstLine + i + taken - 1,
			})
		}

		if i+taken >= len(lines) {
			break
		}

		// Step back enough lines to re-include ~overlap chars, keeping progress.
		back, chars := 0, 0
		for back < taken-1 && chars < overlap {
			chars += len(lines[i+taken-1-back]) + 1
			back++
		}

		i += taken - back
	}

	return out
}

// joinLines joins the 1-based inclusive line range [start, end].
func joinLines(lines []string, start, end int) string {
	return strings.Join(lines[start-1:end], "\n")
}

// capText hard-caps text at maxChunkChars(size).
func capText(text string, size int) string {
	if maxLen := maxChunkChars(size); len(text) > maxLen {
		return text[:maxLen]
	}

	return text
}

// parseLine extracts the 1-based line number from a graphify source_location
// such as "L42", "L42-L60", "line 42" or "42:5". Returns 0 when unparseable.
func parseLine(loc string) int {
	s := strings.TrimSpace(loc)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "line "), "Line ")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "L"), "l")

	// Take the leading digit run.
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}

	if end == 0 {
		return 0
	}

	n, err := strconv.Atoi(s[:end])
	if err != nil || n < 1 {
		return 0
	}

	return n
}
