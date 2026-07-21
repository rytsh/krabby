package rag

import "strings"

// chunk splits markdown into heading-aware, size-capped chunks.
//
// The text is first split into sections at markdown headings (each section
// keeps its heading line). Adjacent sections are then packed greedily into
// chunks of at most size characters; a single oversized section is split into
// fixed windows of size characters with overlap characters of context between
// adjacent windows.
func chunk(text string, size, overlap int) []string {
	if size <= 0 {
		size = 1200
	}

	if overlap < 0 {
		overlap = 0
	}

	if overlap >= size {
		overlap = size / 4
	}

	var chunks []string

	var cur strings.Builder

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			chunks = append(chunks, s)
		}

		cur.Reset()
	}

	for _, section := range splitSections(text) {
		if len(section) > size {
			// Oversized section: emit what we have, then window it.
			flush()

			chunks = append(chunks, window(section, size, overlap)...)

			continue
		}

		if cur.Len() > 0 && cur.Len()+1+len(section) > size {
			flush()
		}

		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}

		cur.WriteString(section)
	}

	flush()

	return chunks
}

// splitSections cuts markdown at heading lines (#..###### followed by a
// space), keeping each heading with the text that follows it.
func splitSections(text string) []string {
	lines := strings.Split(text, "\n")

	var sections []string

	var cur strings.Builder

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			sections = append(sections, s)
		}

		cur.Reset()
	}

	inFence := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track fenced code blocks so a "# comment" inside code is not a heading.
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}

		if !inFence && isHeading(trimmed) {
			flush()
		}

		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}

		cur.WriteString(line)
	}

	flush()

	return sections
}

// isHeading reports whether a trimmed line is a markdown ATX heading.
func isHeading(line string) bool {
	if line == "" || line[0] != '#' {
		return false
	}

	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}

	return i <= 6 && i < len(line) && line[i] == ' '
}

// window splits s into fixed-size chunks with overlap characters of context
// between adjacent windows, cutting on line boundaries where possible.
func window(s string, size, overlap int) []string {
	var out []string

	step := size - overlap

	for start := 0; start < len(s); start += step {
		end := min(start+size, len(s))

		// Prefer to cut at a newline near the end of the window.
		if end < len(s) {
			if nl := strings.LastIndexByte(s[start:end], '\n'); nl > step/2 {
				end = start + nl
			}

			step = end - start - overlap
			if step <= 0 {
				step = end - start
			}
		}

		if c := strings.TrimSpace(s[start:end]); c != "" {
			out = append(out, c)
		}

		if end == len(s) {
			break
		}
	}

	return out
}

// firstHeading returns the text of the first markdown heading, or "".
func firstHeading(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if isHeading(trimmed) {
			return strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
	}

	return ""
}
