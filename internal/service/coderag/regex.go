package coderag

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/rakunlabs/bw"
)

const (
	regexDefaultMaxMatches  = 20
	regexMaxMatchesCeiling  = 100
	regexDefaultMaxFiles    = 2000
	regexMaxContextLines    = 10
	regexDefaultPerPage     = 20
	regexMaxPerPage         = 100
	regexChunkMatchesCeling = 200
)

// RegexOptions configures a regular-expression code search.
type RegexOptions struct {
	// CaseSensitive matches case exactly. The default is
	// case-insensitive; the trigram index is case-folded either way, so
	// neither mode costs more than the other.
	CaseSensitive bool
	// ContextLines is how many source lines to return either side of a
	// match (0-10). Context is bounded by the chunk the match landed in,
	// which is a symbol-sized window, not the whole file.
	ContextLines int
	// MaxMatches caps the matches reported per file (default 20).
	MaxMatches int
	// MaxFiles caps how many matching files are collected before the
	// search reports itself as non-exhaustive (default 2000).
	MaxFiles int
	// Path is an optional glob over the repo-relative source path, applied by
	// the caller through Scope; it travels in the options so every mode takes
	// the filter the same way.
	Path string
	// Page and PerPage paginate over matching files.
	Page    int
	PerPage int
}

// RegexMatch is one occurrence of the pattern, located in the file.
//
// There is deliberately no enclosing-symbol field. The only symbol the
// index knows is the label of the chunk the match landed in, which is the
// first symbol that chunk packed and routinely not the one containing the
// match — a guess beside an exact line is worse than no guess. Use
// query_graph or get_node when the enclosing symbol matters.
type RegexMatch struct {
	Line   int      `json:"line"`
	Column int      `json:"column"`
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// RegexHit groups one file's matches.
type RegexHit struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
	// Matches is ordered by line.
	Matches []RegexMatch `json:"matches"`
	// Truncated reports that the file matched more times than are listed
	// here — the per-file cap trimmed the list, or one chunk of it matched
	// more than the walk reports per chunk. It never means the file is
	// partly unsearched: it means "there are more of these".
	Truncated bool `json:"truncated,omitempty"`
}

// RegexPage is one page of regular-expression search results.
type RegexPage struct {
	Results []RegexHit `json:"results"`
	// Total is the number of matching files, exact unless Exhaustive is
	// false.
	Total   uint64 `json:"total"`
	Page    int    `json:"page"`
	PerPage int    `json:"per_page"`
	// Exhaustive is false when MaxFiles cut the search short, so a
	// caller can tell "these are all of them" from "these are the first
	// of them".
	Exhaustive bool `json:"exhaustive"`
	// Indexed reports the index state of the repositories behind this page.
	Indexed []RepoIndex `json:"indexed,omitempty"`
}

// SearchRegex runs an RE2 regular expression against the indexed source
// chunks, returning matches grouped by file.
//
// The trigram index picks the candidate chunks and the regexp decides the
// matches, so within a chunk the answer is what a linear scan of that chunk
// would give — the index only removes the reading. A pattern with no literal
// run of three bytes (`.`, `[a-z]+`) has nothing to prefilter on and falls
// back to a scan of the scoped chunks.
//
// The unit of matching is the chunk, not the file, and that is visible in two
// places: a match that straddles a chunk boundary is not found (chunks overlap
// by design, so this needs a match longer than the overlap), and context lines
// stop at the chunk's edge. Both are properties of a symbol-windowed index; a
// caller that needs the whole file reads it with read_file.
//
// Patterns are matched in multi-line mode, so `^` and `$` are line
// anchors as they are in grep. Anchoring them to the searched value
// instead would anchor them to a chunk boundary, which is an artefact of
// how the index stores source and means nothing to the caller. `.` still
// does not match a newline unless the pattern asks with `(?s)`.
//
// keyFilter scopes the search by chunk id (the repository is the id's
// first segment); nil searches every repository.
func (s *TextStore) SearchRegex(
	ctx context.Context,
	keyFilter func(string) bool,
	pattern string,
	opts RegexOptions,
) (RegexPage, error) {
	page, perPage := opts.Page, opts.PerPage
	if page < 1 {
		page = 1
	}
	switch {
	case perPage <= 0:
		perPage = regexDefaultPerPage
	case perPage > regexMaxPerPage:
		perPage = regexMaxPerPage
	}

	maxMatches := opts.MaxMatches
	switch {
	case maxMatches <= 0:
		maxMatches = regexDefaultMaxMatches
	case maxMatches > regexMaxMatchesCeiling:
		maxMatches = regexMaxMatchesCeiling
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = regexDefaultMaxFiles
	}
	contextLines := min(max(opts.ContextLines, 0), regexMaxContextLines)

	result := RegexPage{Results: []RegexHit{}, Page: page, PerPage: perPage, Exhaustive: true}
	offset := math.MaxInt
	if page-1 <= math.MaxInt/perPage {
		offset = (page - 1) * perPage
	}

	// Chunk ids sort as "<repo>/<path>#<n>", but "#10" precedes "#2".
	// Keep each page file's earliest matches ordered as chunks arrive.
	var (
		order []string
		// Off-page files have a nil value: they contribute to the exact
		// count, but never retain match text or surrounding source lines.
		files = make(map[string]*RegexHit)
	)

	if _, err := s.bucket.RegexWalk(ctx, "(?m)"+pattern, bw.RegexOptions{
		Field:         "snippet",
		CaseSensitive: opts.CaseSensitive,
		KeyFilter:     keyFilter,
		// Per chunk rather than per file: a file's cap is applied once
		// its chunks have been merged, and a chunk that matches a
		// thousand times still only needs to say so.
		MaxMatches: regexChunkMatchesCeling,
	}, func(hit bw.RegexResult[textRecord]) (bool, error) {
		record := hit.Record
		if record == nil {
			return true, nil
		}

		key := record.Repo + "\x00" + record.Path
		acc, ok := files[key]
		if !ok {
			if len(order) >= maxFiles {
				result.Exhaustive = false

				return false, nil
			}
			index := len(order)
			if index >= offset && index-offset < perPage {
				acc = &RegexHit{Repo: record.Repo, Path: record.Path}
			}
			files[key] = acc
			order = append(order, key)
		}
		if acc == nil {
			return true, nil
		}

		for _, m := range chunkMatches(record, hit.Matches, contextLines) {
			// A file's chunks overlap when a single symbol was too large
			// to fit one, so the same occurrence can arrive twice. Line
			// alone is not the identity: several matches on one line are
			// distinct occurrences and dropping them undercounts silently,
			// while a genuine duplicate repeats the column too.
			acc.addMatch(m, maxMatches)
		}
		if hit.Truncated {
			acc.Truncated = true
		}

		return true, nil
	}); err != nil {
		return result, fmt.Errorf("regex code search; %w", err)
	}

	result.Total = uint64(len(order))

	start := min(offset, len(order))
	end := start + min(perPage, len(order)-start)
	for _, key := range order[start:end] {
		acc := files[key]
		result.Results = append(result.Results, *acc)
	}

	return result, nil
}

// addMatch retains only the earliest limit distinct locations. Chunk key order
// is not source-line order (#10 precedes #2), so merely stopping at the cap
// would return the wrong occurrences. Binary insertion keeps memory bounded
// while allowing a later chunk to replace the furthest retained match.
func (h *RegexHit) addMatch(m RegexMatch, limit int) {
	i, duplicate := slices.BinarySearchFunc(h.Matches, m, func(a, b RegexMatch) int {
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Column - b.Column
	})
	if duplicate {
		return
	}
	if len(h.Matches) == limit {
		h.Truncated = true
		if i == limit {
			return
		}
		copy(h.Matches[i+1:], h.Matches[i:limit-1])
		h.Matches[i] = m
		return
	}
	h.Matches = slices.Insert(h.Matches, i, m)
}

// chunkMatches converts bw's chunk-relative offsets into file-relative
// locations with their surrounding source lines.
func chunkMatches(record *textRecord, matches []bw.RegexMatch, contextLines int) []RegexMatch {
	if len(matches) == 0 {
		return nil
	}

	lines := strings.Split(record.Snippet, "\n")
	// Byte offset of each line, so a match's column is a subtraction
	// rather than a re-scan of the chunk.
	starts := make([]int, len(lines))
	offset := 0
	for i, line := range lines {
		starts[i] = offset
		offset += len(line) + 1
	}

	out := make([]RegexMatch, 0, len(matches))
	for _, m := range matches {
		idx := m.Line - 1
		if idx < 0 || idx >= len(lines) {
			continue
		}
		column := m.Start - starts[idx] + 1
		if column < 1 {
			column = 1
		}

		hit := RegexMatch{
			// StartLine is the file line of the chunk's first line.
			Line:   record.StartLine + idx,
			Column: column,
			Text:   strings.TrimRight(lines[idx], "\r"),
		}
		if contextLines > 0 {
			before := max(idx-contextLines, 0)
			after := min(idx+contextLines+1, len(lines))
			hit.Before = trimLines(lines[before:idx])
			hit.After = trimLines(lines[idx+1 : after])
		}
		out = append(out, hit)
	}

	return out
}

func trimLines(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, line := range in {
		out = append(out, strings.TrimRight(line, "\r"))
	}

	return out
}
