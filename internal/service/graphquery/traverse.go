package graphquery

import (
	"fmt"
	"sort"
	"strings"
)

// Canonical edge-context identifiers stored on edges (match graphify).
const (
	ctxCall      = "call"
	ctxImport    = "import"
	ctxField     = "field"
	ctxParamType = "parameter_type"
	ctxReturn    = "return_type"
	ctxGeneric   = "generic_arg"
	ctxAttribute = "attribute"
	ctxExport    = "export"
)

// jsonPropertiesKey is the shared "properties" literal (JSON noise label + field alias).
const jsonPropertiesKey = "properties"

// contextFilterAliases normalises user-supplied edge-context filter values to the
// canonical context stored on edges (mirrors _CONTEXT_FILTER_ALIASES).
var contextFilterAliases = map[string]string{
	"param": ctxParamType, "params": ctxParamType, "parameter": ctxParamType,
	"parameters": ctxParamType, "argument": ctxParamType, "arguments": ctxParamType,
	"arg": ctxParamType, "args": ctxParamType,
	"return": ctxReturn, "returns": ctxReturn, "returned": ctxReturn,
	"generic": ctxGeneric, "generics": ctxGeneric, "template": ctxGeneric, "templates": ctxGeneric,
	"annotation": ctxAttribute, "annotations": ctxAttribute, "decorator": ctxAttribute, "decorators": ctxAttribute,
	"calls": ctxCall, "called": ctxCall, "invoke": ctxCall, "invocation": ctxCall,
	"fields": ctxField, "property": ctxField, jsonPropertiesKey: ctxField, "member": ctxField, "members": ctxField,
	"imports": ctxImport, "imported": ctxImport, "module": ctxImport, "modules": ctxImport,
	"exports": ctxExport, "exported": ctxExport,
}

// contextHints maps an edge context to question keywords that imply it (mirrors
// _CONTEXT_HINTS). Order matters for deterministic output.
var contextHints = []struct {
	context string
	hints   []string
}{
	{ctxCall, []string{"call", "calls", "called", "invoke", "invokes", "invoked"}},
	{ctxImport, []string{"import", "imports", "imported", "module", "modules"}},
	{ctxField, []string{"field", "fields", "member", "members", "property", "properties"}},
	{ctxParamType, []string{"parameter", "parameters", "param", "params", "argument", "arguments"}},
	{ctxReturn, []string{"return", "returns", "returned"}},
	{ctxGeneric, []string{"generic", "generics", "template", "templates"}},
}

func normalizeContextFilters(filters []string) []string {
	if len(filters) == 0 {
		return nil
	}

	var normalized []string
	seen := map[string]bool{}
	for _, v := range filters {
		key := strings.ToLower(strings.TrimSpace(stripDiacritics(v)))
		if key == "" {
			continue
		}

		if alias, ok := contextFilterAliases[key]; ok {
			key = alias
		}

		if !seen[key] {
			seen[key] = true
			normalized = append(normalized, key)
		}
	}

	return normalized
}

func inferContextFilters(question string) []string {
	lowered := map[string]bool{}
	repl := strings.NewReplacer("?", " ", ",", " ")
	for _, tok := range strings.Fields(repl.Replace(question)) {
		lowered[strings.ToLower(stripDiacritics(tok))] = true
	}

	var inferred []string
	for _, ch := range contextHints {
		for _, h := range ch.hints {
			if lowered[h] {
				inferred = append(inferred, ch.context)

				break
			}
		}
	}

	return inferred
}

// resolveContextFilters returns the effective filters and their source
// ("explicit"/"heuristic") (mirrors _resolve_context_filters).
func resolveContextFilters(question string, explicit []string) ([]string, string) {
	if normalized := normalizeContextFilters(explicit); len(normalized) > 0 {
		return normalized, "explicit"
	}

	if inferred := inferContextFilters(question); len(inferred) > 0 {
		return inferred, "heuristic"
	}

	return nil, ""
}

// filteredNeighbors returns successor edges whose context is in the filter set.
// An empty filter set returns all successors (mirrors _filter_graph_by_context
// applied during traversal over successors). The edge is returned alongside the
// neighbour so callers that render the traversal never have to look it up again.
func (g *Graph) filteredNeighbors(id string, filters map[string]bool) []edgeRef {
	refs := g.out[id]
	out := make([]edgeRef, 0, len(refs))
	for _, r := range refs {
		if len(filters) == 0 || filters[r.edge.Context] {
			out = append(out, r)
		}
	}

	return out
}

// computeHubThreshold returns the degree above which nodes are not expanded as
// transit: p99 of the degree distribution, floored at 50 (mirrors the shared
// helper in _bfs/_dfs). Called once per load; the result lives on Graph.
func computeHubThreshold(g *Graph) int {
	if len(g.nodeList) == 0 {
		return 50
	}

	degrees := make([]int, 0, len(g.nodeList))
	for _, id := range g.nodeList {
		degrees = append(degrees, g.degree[id])
	}
	sort.Ints(degrees)

	p99 := int(float64(len(degrees)) * 0.99)
	if p99 >= len(degrees) {
		p99 = len(degrees) - 1
	}

	return max(50, degrees[p99])
}

// edgePair is one edge recorded during traversal, in visit order. It carries the
// traversed *Edge so rendering does not re-scan the source node's adjacency —
// that lookup made rendering quadratic in the degree of a hub node.
type edgePair struct {
	from string
	to   string
	edge *Edge
}

// bfs performs a breadth-first traversal over context-filtered successors,
// refusing to expand through high-degree hubs (except seeds) (mirrors _bfs).
func (g *Graph) bfs(start []string, depth int, filters map[string]bool) (map[string]bool, []edgePair) {
	hub := g.hubThreshold
	seedSet := toSet(start)
	visited := toSet(start)
	frontier := toSet(start)
	var edges []edgePair

	for range depth {
		next := map[string]bool{}
		// Iterate frontier in insertion order for deterministic edge lists.
		for _, n := range g.orderNodes(frontier) {
			if !seedSet[n] && g.degree[n] >= hub {
				continue
			}

			for _, r := range g.filteredNeighbors(n, filters) {
				if !visited[r.other] {
					next[r.other] = true
					edges = append(edges, edgePair{from: n, to: r.other, edge: r.edge})
				}
			}
		}

		for k := range next {
			visited[k] = true
		}
		frontier = next
	}

	return visited, edges
}

// dfs performs a depth-first traversal mirroring _dfs (LIFO stack, seeds pushed
// in reverse so the first seed is explored first).
func (g *Graph) dfs(start []string, depth int, filters map[string]bool) (map[string]bool, []edgePair) {
	hub := g.hubThreshold
	seedSet := toSet(start)
	visited := map[string]bool{}
	var edges []edgePair

	type frame struct {
		node string
		d    int
	}

	var stack []frame
	for i := len(start) - 1; i >= 0; i-- {
		stack = append(stack, frame{node: start[i], d: 0})
	}

	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if visited[f.node] || f.d > depth {
			continue
		}
		visited[f.node] = true

		if !seedSet[f.node] && g.degree[f.node] >= hub {
			continue
		}

		for _, r := range g.filteredNeighbors(f.node, filters) {
			if !visited[r.other] {
				stack = append(stack, frame{node: r.other, d: f.d + 1})
				edges = append(edges, edgePair{from: f.node, to: r.other, edge: r.edge})
			}
		}
	}

	return visited, edges
}

// subgraphToText renders a subgraph as text, cutting at tokenBudget (~3 chars per
// token). Seeds are rendered first, then remaining nodes by degree desc (mirrors
// _subgraph_to_text). Each edge is rendered from the edge the traversal actually
// followed, so a context filter is honoured here as well as during expansion.
func (g *Graph) subgraphToText(nodes map[string]bool, edges []edgePair, tokenBudget int, seeds []string) string {
	charBudget := tokenBudget * 3
	var lines []string

	seedSet := toSet(seeds)
	// Seeds first (in given order, only if present in the node set).
	for _, s := range seeds {
		if nodes[s] {
			lines = append(lines, g.nodeLine(s))
		}
	}

	// Remaining nodes by degree desc (stable on ties).
	rest := make([]string, 0, len(nodes))
	for _, id := range g.nodeList { // stable base order
		if nodes[id] && !seedSet[id] {
			rest = append(rest, id)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		return g.degree[rest[i]] > g.degree[rest[j]]
	})
	for _, id := range rest {
		lines = append(lines, g.nodeLine(id))
	}

	for _, e := range edges {
		if !nodes[e.from] || !nodes[e.to] {
			continue
		}

		contextSuffix := ""
		if e.edge.Context != "" {
			contextSuffix = " context=" + sanitize(e.edge.Context)
		}

		lines = append(lines, fmt.Sprintf(
			"EDGE %s --%s [%s%s]--> %s",
			sanitize(g.labelOf(e.from)),
			sanitize(e.edge.Relation),
			sanitize(e.edge.Confidence),
			contextSuffix,
			sanitize(g.labelOf(e.to)),
		))
	}

	output := strings.Join(lines, "\n")
	if len(output) <= charBudget {
		return output
	}

	// Truncate on a line boundary and report how many NODE lines were cut.
	cutAt := strings.LastIndex(output[:charBudget], "\n")
	if cutAt <= 0 {
		cutAt = charBudget
	}

	totalNodes := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "NODE ") {
			totalNodes++
		}
	}

	shown := strings.Count(output[:cutAt], "\nNODE ")
	if strings.HasPrefix(output, "NODE ") {
		shown++
	}

	cutCount := totalNodes - shown

	return output[:cutAt] + fmt.Sprintf(
		"\n... (truncated — %d more nodes cut by ~%d-token budget."+
			" Narrow with context_filter=['call'] or use get_node for a specific symbol)",
		cutCount, tokenBudget,
	)
}

func (g *Graph) nodeLine(id string) string {
	d := g.Nodes[id]
	community := ""
	if d.Community != nil {
		community = fmt.Sprintf("%d", *d.Community)
	}

	return fmt.Sprintf(
		"NODE %s [src=%s loc=%s community=%s]",
		sanitize(g.labelOf(id)),
		sanitize(d.SourceFile),
		sanitize(d.SourceLocation),
		sanitize(community),
	)
}

// labelOf returns the node label, falling back to the id (mirrors d.get('label', nid)).
func (g *Graph) labelOf(id string) string {
	if n, ok := g.Nodes[id]; ok && n.Label != "" {
		return n.Label
	}

	return id
}

// orderNodes returns the members of set in the graph's stable insertion order.
func (g *Graph) orderNodes(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for _, id := range g.nodeList {
		if set[id] {
			out = append(out, id)
		}
	}

	return out
}

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}

	return m
}
