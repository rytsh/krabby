package rag

import (
	"strings"
	"unicode"
)

// maxQueryTerms bounds how many terms a built lexical query carries. This is
// the language-independent cost guard: every extra clause makes bw scan that
// term's whole posting list, so an unbounded question would scale query time
// with sentence length.
const maxQueryTerms = 12

// StopWords is a lower-cased set of words to drop from a built lexical query.
// See LexicalQuery for why it is optional and why it must match the corpus
// language. Build one with NewStopWords.
type StopWords map[string]bool

// NewStopWords lower-cases and de-duplicates words into a StopWords set.
// A nil or empty input yields a nil set, which disables filtering.
func NewStopWords(words []string) StopWords {
	set := make(StopWords, len(words))

	for _, word := range words {
		if word = strings.ToLower(strings.TrimSpace(word)); word != "" {
			set[word] = true
		}
	}

	if len(set) == 0 {
		return nil
	}

	return set
}

// Merge returns the union of s and other. Either may be nil.
func (s StopWords) Merge(other StopWords) StopWords {
	switch {
	case len(s) == 0:
		return other
	case len(other) == 0:
		return s
	}

	merged := make(StopWords, len(s)+len(other))
	for _, set := range []StopWords{s, other} {
		for word := range set {
			merged[word] = true
		}
	}

	return merged
}

// LexicalQuery turns a user question into a bw full-text query.
//
// bw's default is to AND every term (see its query grammar), so passing a
// natural-language sentence verbatim requires all of its words — including
// "how", "the" and "does" — to appear inside a single chunk, which almost
// always matches nothing. This builder instead:
//
//   - passes the string through untouched when it already uses bw operators
//     ('"', OR, NOT, a leading '-' or a trailing '*'), so power users keep
//     full control;
//   - keeps identifier-like words (containing a digit or one of . - _ / + @,
//     e.g. PAY-1842, ERR_CAPTURE_42, v0.39.0) as required AND clauses so exact
//     keys stay precise;
//   - drops single-character words and any word in stop;
//   - ORs the remaining prose words, letting BM25 rank documents by how many
//     of them (and how rarely) they match.
//
// The OR chain is emitted before the required terms because bw attaches an OR
// to the preceding positive clause; emitting a required term first would pull
// it into the OR group and lose its "must match" property.
//
// stop is optional and defaults to empty. It is deliberately not a built-in
// list: BM25's IDF already drives a term appearing in most documents to a
// near-zero score in every language, so filtering stop words buys latency, not
// relevance. A built-in list would therefore trade correctness for speed —
// it would only ever cover a couple of languages, and generic entries like
// "get" or "use" are meaningful terms in technical documentation. Deployments
// with a large corpus can supply a list matching their own corpus language;
// maxQueryTerms bounds the cost regardless.
//
// When nothing survives filtering the original question is returned unchanged.
func LexicalQuery(question string, stop StopWords) string {
	trimmed := strings.TrimSpace(question)
	if trimmed == "" || hasQueryOperators(trimmed) {
		return trimmed
	}

	var required, optional []string

	for _, word := range strings.Fields(trimmed) {
		word = strings.Trim(word, ",;:!?()[]{}<>\"'`")
		if word == "" {
			continue
		}

		if isIdentifierLike(word) {
			required = appendUnique(required, word)

			continue
		}

		lower := strings.ToLower(word)
		if len([]rune(lower)) < 2 || stop[lower] {
			continue
		}

		optional = appendUnique(optional, lower)
	}

	if len(required)+len(optional) == 0 {
		return trimmed
	}

	// Budget: required terms are the high-signal ones, so they are never
	// dropped; prose terms fill whatever is left.
	if len(required) > maxQueryTerms {
		required = required[:maxQueryTerms]
	}

	if budget := maxQueryTerms - len(required); len(optional) > budget {
		optional = optional[:max(budget, 0)]
	}

	parts := make([]string, 0, len(required)+1)
	if len(optional) > 0 {
		parts = append(parts, strings.Join(optional, " OR "))
	}

	parts = append(parts, required...)

	return strings.Join(parts, " ")
}

// hasQueryOperators reports whether s already uses bw query syntax, in which
// case it must be forwarded verbatim.
func hasQueryOperators(s string) bool {
	if strings.ContainsAny(s, `"`) {
		return true
	}

	for _, word := range strings.Fields(s) {
		switch word {
		case "OR", "NOT":
			return true
		}

		if strings.HasPrefix(word, "-") && len(word) > 1 {
			return true
		}

		if strings.HasSuffix(word, "*") && len(word) > 1 {
			return true
		}
	}

	return false
}

// isIdentifierLike reports whether word looks like a key, version, error code
// or path rather than prose: it contains a digit, or punctuation that bw's
// tokenizer treats as a compound joiner.
func isIdentifierLike(word string) bool {
	hasLetterOrDigit := false

	for _, r := range word {
		switch {
		case unicode.IsDigit(r):
			return true
		case r == '.' || r == '-' || r == '_' || r == '/' || r == '+' || r == '@':
			return true
		case unicode.IsLetter(r):
			hasLetterOrDigit = true
		}
	}

	return !hasLetterOrDigit
}

func appendUnique(terms []string, term string) []string {
	for _, existing := range terms {
		if strings.EqualFold(existing, term) {
			return terms
		}
	}

	return append(terms, term)
}
