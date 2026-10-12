package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Pin records the current state of the evidenced lines. A file-level pin (no
// line range) only tracks the file's existence: hashing a whole file would
// mark the memory stale on every unrelated edit.
func Pin(e Evidence, content string) (Evidence, bool) {
	if e.StartLine == 0 {
		e.Hash = ""
		return e, true
	}
	lines := splitLines(content)
	if e.EndLine > len(lines) {
		return e, false
	}
	e.Hash = hashLines(lines[e.StartLine-1 : e.EndLine])
	return e, true
}

// Check compares pinned evidence with the file's current content (exists
// false means the file is gone). The returned evidence carries the new status
// and, for moved code, the new line range.
func Check(e Evidence, content string, exists bool) Evidence {
	if !exists {
		e.Status = EvidenceMissing
		return e
	}
	if e.StartLine == 0 || e.Hash == "" {
		e.Status = EvidenceOK
		return e
	}
	lines := splitLines(content)
	span := e.EndLine - e.StartLine + 1
	if e.EndLine <= len(lines) && hashLines(lines[e.StartLine-1:e.EndLine]) == e.Hash {
		e.Status = EvidenceOK
		return e
	}
	// Unchanged code that shifted: find the same block nearest to the
	// original position.
	best := -1
	for start := 0; start+span <= len(lines); start++ {
		if hashLines(lines[start:start+span]) != e.Hash {
			continue
		}
		if best < 0 || abs(start+1-e.StartLine) < abs(best+1-e.StartLine) {
			best = start
		}
	}
	if best >= 0 {
		e.StartLine, e.EndLine = best+1, best+span
		e.Status = EvidenceMoved
		return e
	}
	e.Status = EvidenceChanged
	return e
}

func splitLines(content string) []string {
	content = strings.TrimSuffix(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// hashLines ignores trailing whitespace so an editor's whitespace cleanup does
// not mark a memory stale.
func hashLines(lines []string) string {
	h := sha256.New()
	for _, line := range lines {
		h.Write([]byte(strings.TrimRight(line, " \t")))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
