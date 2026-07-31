package rag

import "strings"

// IndexProjectionVersion changes whenever the text sent to the lexical or
// semantic indexes changes. Startup schedules one full reindex when it sees a
// newer version than the one persisted in settings.
const IndexProjectionVersion = 1

// searchableMarkdown keeps the human-visible part of inline links and images
// while dropping their destinations. Full markdown stays on disk for display;
// this projection only feeds chunking and search indexes.
func searchableMarkdown(markdown string, keepTargets bool) string {
	if keepTargets {
		return markdown
	}
	markdown = withoutReferenceDefinitions(markdown)

	var out strings.Builder
	out.Grow(len(markdown))
	inFence := byte(0)
	fenceLen := 0

	for i := 0; i < len(markdown); {
		if i == 0 || markdown[i-1] == '\n' {
			lineEnd := strings.IndexByte(markdown[i:], '\n')
			if lineEnd < 0 {
				lineEnd = len(markdown) - i
			} else {
				lineEnd++
			}
			line := markdown[i : i+lineEnd]
			if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
				out.WriteString(line)
				i += lineEnd
				continue
			}
			marker, count := markdownFence(line)
			if inFence != 0 {
				out.WriteString(line)
				i += lineEnd
				if marker == inFence && count >= fenceLen {
					inFence, fenceLen = 0, 0
				}
				continue
			}
			if marker != 0 {
				inFence, fenceLen = marker, count
				out.WriteString(line)
				i += lineEnd
				continue
			}
		}
		if markdown[i] == '\\' && i+1 < len(markdown) {
			out.WriteString(markdown[i : i+2])
			i += 2
			continue
		}

		if markdown[i] == '`' {
			run := delimiterRun(markdown, i, '`')
			if end := strings.Index(markdown[i+run:], strings.Repeat("`", run)); end >= 0 {
				end += i + run*2
				out.WriteString(markdown[i:end])
				i = end
				continue
			}
		}

		start := i
		if markdown[i] == '!' && i+1 < len(markdown) && markdown[i+1] == '[' {
			start++
		}

		if markdown[start] == '[' {
			labelEnd, ok := matchingDelimiter(markdown, start, '[', ']')
			if ok && labelEnd+1 < len(markdown) && markdown[labelEnd+1] == '(' {
				destinationEnd, destinationOK := matchingDelimiter(markdown, labelEnd+1, '(', ')')
				if destinationOK {
					out.WriteString(searchableMarkdown(markdown[start+1:labelEnd], false))
					i = destinationEnd + 1

					continue
				}
			}
			if ok && labelEnd+1 < len(markdown) && markdown[labelEnd+1] == '[' {
				referenceEnd, referenceOK := matchingDelimiter(markdown, labelEnd+1, '[', ']')
				if referenceOK {
					out.WriteString(searchableMarkdown(markdown[start+1:labelEnd], false))
					i = referenceEnd + 1
					continue
				}
			}
		}

		// Autolinks expose only their URL, so they carry no useful label once
		// destinations are intentionally excluded from search.
		if markdown[i] == '<' && (hasPrefixFold(markdown[i+1:], "http://") || hasPrefixFold(markdown[i+1:], "https://")) {
			if end := strings.IndexByte(markdown[i+1:], '>'); end >= 0 {
				i += end + 2

				continue
			}
		}
		// Raw HTML tags are not visible search text. Dropping the complete tag
		// also removes href/src targets without attempting to parse HTML here.
		if markdown[i] == '<' && i+1 < len(markdown) && isHTMLTagStart(markdown[i+1]) {
			if end := strings.IndexByte(markdown[i+1:], '>'); end >= 0 {
				i += end + 2
				continue
			}
		}

		out.WriteByte(markdown[i])
		i++
	}

	return out.String()
}

func withoutReferenceDefinitions(markdown string) string {
	var out strings.Builder
	inFence := byte(0)
	fenceLen := 0
	wantDestination := false
	wantTitle := false
	for len(markdown) > 0 {
		lineEnd := strings.IndexByte(markdown, '\n')
		if lineEnd < 0 {
			lineEnd = len(markdown)
		} else {
			lineEnd++
		}
		line := markdown[:lineEnd]
		markdown = markdown[lineEnd:]
		marker, count := markdownFence(line)
		if inFence != 0 {
			out.WriteString(line)
			if marker == inFence && count >= fenceLen {
				inFence, fenceLen = 0, 0
			}
			continue
		}
		if marker != 0 {
			inFence, fenceLen = marker, count
			out.WriteString(line)
			continue
		}
		trimmed := strings.TrimSpace(line)
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		if wantDestination {
			wantDestination = false
			if indented && trimmed != "" {
				wantTitle = true
				continue
			}
		}
		if wantTitle {
			wantTitle = false
			if indented && isReferenceTitle(trimmed) {
				continue
			}
		}
		leftTrimmed := strings.TrimLeft(line, " \t")
		if len(line)-len(leftTrimmed) <= 3 && strings.HasPrefix(leftTrimmed, "[") {
			if end, ok := matchingDelimiter(leftTrimmed, 0, '[', ']'); ok && end+1 < len(leftTrimmed) && leftTrimmed[end+1] == ':' {
				rest := strings.TrimSpace(leftTrimmed[end+2:])
				wantDestination = rest == ""
				wantTitle = rest != ""
				continue
			}
		}
		out.WriteString(line)
	}
	return out.String()
}

func isReferenceTitle(line string) bool {
	if len(line) < 2 {
		return false
	}
	return line[0] == '"' || line[0] == '\'' || line[0] == '('
}

func markdownFence(line string) (byte, int) {
	line = strings.TrimLeft(line, " \t")
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	count := delimiterRun(line, 0, line[0])
	if count < 3 {
		return 0, 0
	}
	return line[0], count
}

func delimiterRun(s string, start int, delimiter byte) int {
	i := start
	for i < len(s) && s[i] == delimiter {
		i++
	}
	return i - start
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func isHTMLTagStart(c byte) bool {
	return c == '/' || c == '!' || c == '?' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func matchingDelimiter(text string, start int, open, close byte) (int, bool) {
	depth := 0
	for i := start; i < len(text); i++ {
		if text[i] == '\\' {
			i++

			continue
		}

		switch text[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}

	return 0, false
}

// withSearchTitle gives every semantic chunk document-level context. It also
// makes lexical excerpts self-contained without duplicating an existing first
// heading that already matches the authoritative title.
func withSearchTitle(text, title string) string {
	text = strings.TrimSpace(text)
	title = strings.TrimSpace(title)
	if title == "" || strings.EqualFold(firstHeading(text), title) {
		return text
	}

	return "# " + title + "\n\n" + text
}
