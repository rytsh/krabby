package graphquery

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ---- label sanitisation (mirrors graphify.security.sanitize_label) ----------

const maxLabelLen = 256

var controlCharRe = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// sanitize strips control characters and caps length. Every LLM-derived field is
// passed through this before being concatenated into tool output (F-010).
func sanitize(text string) string {
	text = controlCharRe.ReplaceAllString(text, "")
	if len(text) > maxLabelLen {
		text = text[:maxLabelLen]
	}

	return text
}

// ---- tokenisation (mirrors graphify.serve helpers) --------------------------

var wordRe = regexp.MustCompile(`\w+`)

// stripDiacritics removes combining marks after NFKD normalisation.
func stripDiacritics(text string) string {
	decomposed := norm.NFKD.String(text)
	var b strings.Builder
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) { // Mn = nonspacing combining mark
			continue
		}
		b.WriteRune(r)
	}

	return b.String()
}

// searchTokens splits text into lowercase word tokens, stripping punctuation and
// diacritics (mirrors _search_tokens).
func searchTokens(text string) []string {
	return wordRe.FindAllString(strings.ToLower(stripDiacritics(text)), -1)
}

func hasChinese(text string) bool {
	for _, ch := range text {
		if ch >= '\u4e00' && ch <= '\u9fff' {
			return true
		}
	}

	return false
}

// segmentChinese mirrors _segment_chinese without jieba: bigram segmentation
// plus the original term for exact matching.
func segmentChinese(text string) []string {
	runes := []rune(text)
	var segments []string
	for i := 0; i+1 < len(runes); i++ {
		segments = append(segments, string(runes[i:i+2]))
	}

	if len(segments) == 0 {
		segments = []string{text}
	}

	if len(runes) > 1 {
		found := false
		for _, s := range segments {
			if s == text {
				found = true

				break
			}
		}

		if !found {
			segments = append(segments, text)
		}
	}

	return segments
}

// isSearchable mirrors _is_searchable: Chinese/non-English terms are always
// searchable; pure-ASCII-lowercase terms must exceed 2 chars.
func isSearchable(term string) bool {
	allAZ := true
	for _, ch := range term {
		if ch < 'a' || ch > 'z' {
			allAZ = false

			break
		}
	}

	if allAZ {
		return len(term) > 2
	}

	return true
}

// queryTerms splits a query into searchable terms, segmenting Chinese text
// (mirrors _query_terms).
func queryTerms(question string) []string {
	var terms []string
	for _, raw := range strings.Fields(question) {
		if hasChinese(raw) {
			for _, seg := range segmentChinese(strings.TrimSpace(strings.ToLower(raw))) {
				seg = strings.TrimSpace(seg)
				if seg != "" && isSearchable(seg) {
					terms = append(terms, seg)
				}
			}

			continue
		}

		for _, tok := range wordRe.FindAllString(strings.ToLower(raw), -1) {
			if isSearchable(tok) {
				terms = append(terms, tok)
			}
		}
	}

	return terms
}

// normLabelOf returns the node's precomputed norm_label, falling back to
// stripDiacritics(label) lowercased (mirrors the fallback used throughout serve.py).
func normLabelOf(n *Node) string {
	if n.NormLabel != "" {
		return n.NormLabel
	}

	return strings.ToLower(stripDiacritics(n.Label))
}
