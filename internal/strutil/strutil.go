// Package strutil holds small string helpers shared across packages that have
// no other reason to depend on one another.
package strutil

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Ellipsis is appended by Truncate to mark elided content.
const Ellipsis = "…"

// Truncate shortens s to at most n bytes of content, appending Ellipsis when
// anything was removed.
//
// The cut lands on a rune boundary. Four packages previously carried their own
// copy of this helper, and every one of them sliced with s[:n], which splits a
// multi-byte rune whenever the limit falls inside one. These limits are applied
// to upstream error bodies and issue summaries — JIRA, Confluence and OpenAPI
// documents that are routinely not ASCII — so the truncated value reached the
// UI with a mangled trailing character.
//
// n <= 0 yields the empty string.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}

	// Walk back to the start of the rune that the byte limit lands inside.
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + Ellipsis
}

// TruncateSpace trims surrounding whitespace before truncating, for values that
// arrive padded (command output, free-text fields).
func TruncateSpace(s string, n int) string {
	return Truncate(strings.TrimSpace(s), n)
}

// ErrorExcerpt renders a failed response's body for an error message: a short
// excerpt when it is a structured error (a JSON object, as APIs answer a bad
// token or a missing page with), and only its size and type otherwise.
//
// The fetchers that use this take URLs from configuration, and the config
// preview tools run them on demand. Echoing an arbitrary body would turn a
// preview into a way to read whatever the URL points at, an internal service
// or a cloud metadata endpoint included, a few hundred bytes at a time. A
// JSON error object is what diagnosing a failed fetch needs; an HTML page or
// a plain-text document is not.
func ErrorExcerpt(body []byte, contentType string, n int) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "(empty body)"
	}
	if strings.HasPrefix(trimmed, "{") && (contentType == "" || strings.Contains(strings.ToLower(contentType), "json")) {
		return Truncate(trimmed, n)
	}
	if contentType == "" {
		contentType = "unknown type"
	}

	return fmt.Sprintf("(%d-byte %s body not shown)", len(body), contentType)
}
