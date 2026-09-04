package graphquery

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Symbol navigation bounds. The candidate list is capped so an ambiguous symbol
// stays readable, and page sizes match pageBounds (default 50, hard max 200).
const (
	maxSymbolCandidates = 20
	defaultSymbolLimit  = 50
	maxSymbolLimit      = 200
)

// Derived node classes for symbol navigation. Only kindSymbol is a definition:
// the other three are graphify's structural noise (a file placeholder, a
// source-less concept, a JSON key), and returning them as "definitions" is
// what made graph lookups useless for navigation. See definitionKind for why
// navigation cannot reuse god_nodes' classification.
const (
	kindFile    = "file"
	kindConcept = "concept"
	kindJSONKey = "json_key"
	kindSymbol  = "symbol"
)

// Definition is one place a symbol is defined, located in the source.
//
// There is no kind field. Only nodes classified as symbols are returned at
// all - the other classes are reported as the rejected candidates that
// explain an empty result - so a kind here would be the constant "symbol" on
// every element of every response, which is a token of context per definition
// spent on something a caller cannot branch on.
type Definition struct {
	Label     string `json:"label"`
	ID        string `json:"id"`
	Path      string `json:"path,omitempty"`     // repo-relative source_file
	Line      int    `json:"line,omitempty"`     // parsed from source_location
	EndLine   int    `json:"end_line,omitempty"` // only set for a range location
	Degree    int    `json:"degree"`
	Community *int   `json:"community,omitempty"`
}

// Reference is one place a symbol is used.
type Reference struct {
	Label      string `json:"label"` // the referring node
	ID         string `json:"id"`
	Path       string `json:"path,omitempty"`
	Line       int    `json:"line,omitempty"`
	Relation   string `json:"relation"`
	Context    string `json:"context,omitempty"`
	Confidence string `json:"confidence,omitempty"`
}

// SymbolDefs carries the resolution outcome alongside the definitions found.
type SymbolDefs struct {
	Symbol      string       `json:"symbol"`
	Definitions []Definition `json:"definitions"`
	Total       int          `json:"total"`
	// Candidates names the weaker-tier matches a fuzzy resolution also found, so
	// an ambiguous symbol is visible rather than silently narrowed. Entries carry
	// the node id when it differs from the label, because labels are not unique.
	Candidates []string `json:"candidates,omitempty"`
	// Note carries the actionable next step when nothing matched.
	Note string `json:"note,omitempty"`
}

// SymbolRefs carries the resolution outcome alongside one page of references.
type SymbolRefs struct {
	Symbol     string      `json:"symbol"`
	Resolved   string      `json:"resolved,omitempty"` // node id the references belong to
	References []Reference `json:"references"`
	Total      int         `json:"total"`
	Page       int         `json:"page"`
	PerPage    int         `json:"per_page"`
	HasMore    bool        `json:"has_more"`
	Candidates []string    `json:"candidates,omitempty"`
	Note       string      `json:"note,omitempty"`
}

// Definitions returns every node that defines symbol. A symbol defined in three
// files yields three definitions: narrowing to one hides the real ambiguity, and
// picking by score is a guess the caller can make better. File, concept and
// JSON-key nodes are not definitions and are excluded, which is reported in Note
// when the exclusion is what emptied the result.
func (g *Graph) Definitions(symbol string, limit int) SymbolDefs {
	if limit <= 0 {
		limit = defaultSymbolLimit
	}
	if limit > maxSymbolLimit {
		limit = maxSymbolLimit
	}

	out := SymbolDefs{Symbol: symbol, Definitions: []Definition{}}

	hits, weaker := g.resolveSymbol(symbol)
	if len(hits) == 0 {
		out.Note = regexFallbackNote(symbol)

		return out
	}

	out.Candidates = g.candidateList(weaker)

	defs := make([]Definition, 0, len(hits))
	var excluded []string
	for _, id := range hits {
		kind := g.definitionKind(id)
		if kind != kindSymbol {
			if len(excluded) < maxSymbolCandidates {
				excluded = append(excluded, kind+" "+sanitize(g.labelOf(id)))
			}

			continue
		}

		d := g.Nodes[id]
		line, endLine := parseSourceLocation(d.SourceLocation)
		def := Definition{
			Label:   g.labelOf(id),
			ID:      id,
			Path:    d.SourceFile,
			Line:    line,
			EndLine: endLine,
			Degree:  g.Degree(id),
		}
		if d.Community != nil {
			// Copy: the graph is shared and cached across queries.
			community := *d.Community
			def.Community = &community
		}
		defs = append(defs, def)
	}

	out.Total = len(defs)
	if len(defs) > limit {
		defs = defs[:limit]
	}
	out.Definitions = defs

	if len(defs) == 0 {
		out.Note = fmt.Sprintf(
			"'%s' matched %d node(s), but all of them are structural nodes, not definitions: %s."+
				" Use get_node for one of those, or search_code with mode='regex' and pattern='\\b%s\\b' to find the source.",
			sanitize(symbol), len(hits), strings.Join(excluded, ", "), sanitize(regexp.QuoteMeta(symbol)))
	}

	return out
}

// References returns one page of the incoming edges of the node symbol resolves
// to — every place the graph records the symbol being used — each rendered with
// the referring node's own path and line so a caller can jump straight there.
// contexts filters on the edge context through the same alias table the traversal
// filters use; a filter matching none of the node's incoming contexts returns the
// node's real context vocabulary in Note, because an empty list here reads as
// "this symbol has no callers" and is a false negative.
func (g *Graph) References(symbol string, contexts []string, page, perPage int) SymbolRefs {
	out := SymbolRefs{Symbol: symbol, References: []Reference{}}

	hits, weaker := g.resolveSymbol(symbol)
	if len(hits) == 0 {
		out.Page, out.PerPage, _, _ = pageBounds(0, page, perPage)
		out.Note = regexFallbackNote(symbol)

		return out
	}

	nid := hits[0]
	out.Resolved = nid

	// Other same-tier matches are candidates too: references belong to exactly one
	// node, so every equally-strong match not chosen must stay visible.
	alternates := make([]string, 0, len(hits)-1+len(weaker))
	alternates = append(alternates, hits[1:]...)
	alternates = append(alternates, weaker...)
	out.Candidates = g.candidateList(alternates)

	normalized := normalizeContextFilters(contexts)
	filters := toSet(normalized)
	if len(filters) > 0 {
		vocab := g.incomingContexts(nid)
		matched := false
		for _, c := range vocab {
			if filters[c] {
				matched = true

				break
			}
		}

		if !matched {
			out.Page, out.PerPage, _, _ = pageBounds(0, page, perPage)
			out.Note = fmt.Sprintf("No incoming edge with context %s on '%s'. %s",
				formatLabelList(normalized), sanitize(g.labelOf(nid)), g.contextVocabularyNote(nid, vocab))

			return out
		}
	}

	refs := make([]Reference, 0, len(g.Predecessors(nid)))
	for _, r := range g.Predecessors(nid) {
		if len(filters) > 0 && !filters[r.edge.Context] {
			continue
		}

		d := g.Nodes[r.other]
		line, _ := parseSourceLocation(d.SourceLocation)
		refs = append(refs, Reference{
			Label:      g.labelOf(r.other),
			ID:         r.other,
			Path:       d.SourceFile,
			Line:       line,
			Relation:   r.edge.Relation,
			Context:    r.edge.Context,
			Confidence: r.edge.Confidence,
		})
	}

	out.Total = len(refs)
	resolvedPage, resolvedPerPage, start, end := pageBounds(len(refs), page, perPage)
	out.Page, out.PerPage = resolvedPage, resolvedPerPage
	out.References = refs[start:end]
	out.HasMore = end < len(refs)

	if len(refs) == 0 {
		out.Note = fmt.Sprintf(
			"'%s' resolved to node '%s', which has no incoming edges: the graph records no use of it."+
				" A definition with no extracted callers is common for entry points and dynamic dispatch —"+
				" confirm with search_code mode='regex' pattern='\\b%s\\b'.",
			sanitize(symbol), sanitize(nid), sanitize(regexp.QuoteMeta(symbol)))
	}

	return out
}

// resolveSymbol returns the nodes matching symbol at the strongest tier that
// matched, plus the weaker-tier matches. The tiers stay separate so a substring
// match can never dilute an exact one; callers report the weaker ones as
// candidates instead of mixing them into results.
func (g *Graph) resolveSymbol(symbol string) (hits, weaker []string) {
	exact, prefix, substring := g.findNodeTiers(strings.ToLower(symbol))
	exact, prefix, substring = g.promoteMethodLabels(symbol, exact, prefix, substring)

	switch {
	case len(exact) > 0:
		return exact, append(prefix, substring...)
	case len(prefix) > 0:
		return prefix, substring
	default:
		return substring, nil
	}
}

// promoteMethodLabels moves nodes whose label is the graph's method form of
// symbol — ".Name()" — into the exact tier.
//
// The shared resolver compares a tokenised query term against the stored
// norm_label verbatim, which is deliberately bug-for-bug with graphify's own
// _find_node and is not changed here. The consequence for navigation is that
// asking for "BlameRepoFile" never matches ".blamerepofile()" exactly, so
// every method lookup fell through to the substring tier — where the caller
// asking for a method got whichever test function happened to contain its name
// first. Definitions survived that (it returns every node of the tier it
// matched); References picks one node and did not.
func (g *Graph) promoteMethodLabels(symbol string, exact, prefix, substring []string) ([]string, []string, []string) {
	want := strings.ToLower(strings.TrimSpace(symbol))
	if want == "" {
		return exact, prefix, substring
	}

	keep := func(ids []string) []string {
		out := ids[:0:0]
		for _, id := range ids {
			if bareMethodLabel(normLabelOf(g.Nodes[id])) == want {
				exact = append(exact, id)

				continue
			}
			out = append(out, id)
		}

		return out
	}

	// keep must run before exact is returned: the operands of a return are
	// evaluated left to right, so reading exact in the same statement would
	// read it before the promotions were appended.
	prefix, substring = keep(prefix), keep(substring)

	return exact, prefix, substring
}

// bareMethodLabel strips the punctuation graphify wraps a method label in.
func bareMethodLabel(label string) string {
	return strings.TrimSuffix(strings.TrimPrefix(label, "."), "()")
}

// definitionKind classifies a node for symbol navigation.
//
// It deliberately does not reuse god_nodes' isFileNode. That classifier counts
// a label like ".Method()" as file-ish noise on purpose, and applying the same
// rule here excluded every method in the graph — the single most common
// definition shape there is. What makes a node a definition is narrower and
// simpler: it names a location. It carries a source_file, it is not that
// file's own placeholder node, and it is not one of the generic JSON keys
// graphify emits for config files.
func (g *Graph) definitionKind(id string) string {
	d := g.Nodes[id]
	switch {
	case d == nil || d.SourceFile == "":
		return kindConcept
	case g.isJSONKeyNode(id):
		return kindJSONKey
	case d.Label == filepath.Base(d.SourceFile):
		return kindFile
	default:
		return kindSymbol
	}
}

// contextVocabularyNote explains what a caller may filter on instead.
//
// The three cases are genuinely different answers and collapsing them is how a
// caller concludes a function has no callers. Not every edge carries a context:
// graphify emits the declaring relation (a method on its receiver, say) with a
// relation and no context at all, so a node can have real incoming edges and an
// empty context vocabulary at the same time.
func (g *Graph) contextVocabularyNote(id string, vocab []string) string {
	if len(vocab) > 0 {
		return fmt.Sprintf("Its actual reference contexts are: %s. Retry with one of those, or omit contexts.",
			sanitize(strings.Join(vocab, ", ")))
	}

	relations := g.incomingRelations(id)
	if len(relations) == 0 {
		return "It has no incoming edges at all, so the graph records no use of it."
	}

	return fmt.Sprintf("Its incoming edges carry no context, only these relations: %s. Omit contexts to see them.",
		sanitize(strings.Join(relations, ", ")))
}

// incomingRelations returns the distinct relations on a node's incoming edges.
func (g *Graph) incomingRelations(id string) []string {
	seen := map[string]bool{}
	for _, r := range g.in[id] {
		if r.edge.Relation != "" {
			seen[r.edge.Relation] = true
		}
	}

	out := make([]string, 0, len(seen))
	for relation := range seen {
		out = append(out, relation)
	}
	sort.Strings(out)

	return out
}

// incomingContexts returns the distinct edge contexts on a node's incoming edges,
// sorted for deterministic output. This is the vocabulary a caller may pass as
// contexts to References.
func (g *Graph) incomingContexts(id string) []string {
	seen := map[string]bool{}
	for _, r := range g.in[id] {
		if r.edge.Context != "" {
			seen[r.edge.Context] = true
		}
	}

	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)

	return out
}

// candidateList renders bounded, deduplicated candidate entries. The node id is
// appended when it differs from the label, since duplicate labels are exactly the
// case where the caller needs an id to re-query precisely.
func (g *Graph) candidateList(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, min(len(ids), maxSymbolCandidates))
	for _, id := range ids {
		label := sanitize(g.labelOf(id))
		entry := label
		if !strings.EqualFold(label, id) {
			entry = label + " (" + sanitize(id) + ")"
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true

		out = append(out, entry)
		if len(out) == maxSymbolCandidates {
			break
		}
	}

	return out
}

// regexFallbackNote is the next action for a symbol the graph does not model.
// graphquery deliberately has no text/regex fallback of its own: reaching into
// the text index from here would couple the graph engine to the search surface,
// so the caller is handed the tool that owns it.
func regexFallbackNote(symbol string) string {
	return fmt.Sprintf(
		"No graph node matches '%s'. The graph only models extracted symbols, so a local, generated or macro name can be missing:"+
			" retry with search_code mode='regex' and pattern='\\b%s\\b'.",
		sanitize(symbol), sanitize(regexp.QuoteMeta(symbol)))
}

// parseSourceLocation extracts the 1-based start and end line from a graphify
// source_location. graphify emits several shapes ("L42", "L42-L60", "line 42",
// "42:5"); unparseable values yield 0 so callers omit the line rather than
// pointing at line 1. Only '-' opens a range: the trailing number in "42:5" is a
// column, and reporting it as an end line would render a backwards span.
func parseSourceLocation(loc string) (line, endLine int) {
	line, rest := leadingLineNumber(strings.TrimSpace(loc))
	if line == 0 {
		return 0, 0
	}

	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "-") {
		return line, 0
	}

	end, _ := leadingLineNumber(strings.TrimSpace(rest[1:]))
	if end < line {
		return line, 0
	}

	return line, end
}

// leadingLineNumber strips one optional "line "/"L" prefix and returns the
// leading digit run plus the unconsumed tail.
func leadingLineNumber(s string) (int, string) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "line "), "Line ")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "L"), "l")

	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}

	if end == 0 {
		return 0, s
	}

	n, err := strconv.Atoi(s[:end])
	if err != nil || n < 1 {
		return 0, s[end:]
	}

	return n, s[end:]
}
