package graphquery

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
)

// Traversal modes accepted by QueryGraph.
const (
	ModeBFS = "bfs"
	ModeDFS = "dfs"
)

// QueryGraphOpts configures QueryGraph.
type QueryGraphOpts struct {
	Mode          string // ModeBFS (default) or ModeDFS; see NormalizeTraversalMode
	Depth         int    // clamped to 1..6, default 3
	TokenBudget   int    // default 2000
	ContextFilter []string
}

// NormalizeTraversalMode resolves a caller-supplied traversal mode. Empty means
// the default, BFS. An unknown non-empty value is an error rather than a
// fall-through to BFS: mode='depth-first' would otherwise return a perfectly
// plausible breadth-first subgraph under a header naming the traversal the
// caller asked for, and nothing in the answer would reveal that the trace they
// requested never ran. Surfaces that can report a Go error should call this
// before dispatching; QueryGraph repeats the check so no path reaches the
// traversal with an unvalidated mode.
func NormalizeTraversalMode(mode string) (string, error) {
	switch mode {
	case "":
		return ModeBFS, nil
	case ModeBFS, ModeDFS:
		return mode, nil
	default:
		return "", fmt.Errorf("mode must be %s or %s", ModeBFS, ModeDFS)
	}
}

// QueryGraph searches the graph via BFS/DFS and renders the subgraph as text
// (mirrors _query_graph_text + _tool_query_graph).
func (g *Graph) QueryGraph(question string, opts QueryGraphOpts) string {
	mode, err := NormalizeTraversalMode(opts.Mode)
	if err != nil {
		return fmt.Sprintf("Invalid mode '%s': %v", sanitize(opts.Mode), err)
	}

	depth := opts.Depth
	if depth <= 0 {
		depth = 3
	}
	if depth > 6 {
		depth = 6
	}

	budget := opts.TokenBudget
	if budget <= 0 {
		budget = 2000
	}
	if budget > 4000 {
		budget = 4000
	}

	terms := queryTerms(question)
	scored := g.scoreNodes(terms)
	start := pickSeeds(scored)
	if len(start) == 0 {
		return "No matching nodes found."
	}

	resolved, source := resolveContextFilters(question, opts.ContextFilter)
	filterSet := toSet(resolved)

	var nodes map[string]bool
	var edges []edgePair
	if mode == ModeDFS {
		nodes, edges = g.dfs(start, depth, filterSet)
	} else {
		nodes, edges = g.bfs(start, depth, filterSet)
	}

	startLabels := make([]string, len(start))
	for i, s := range start {
		startLabels[i] = g.labelOf(s)
	}

	parts := []string{
		fmt.Sprintf("Traversal: %s depth=%d", strings.ToUpper(mode), depth),
		fmt.Sprintf("Start: %s", formatLabelList(startLabels)),
	}
	if len(resolved) > 0 {
		parts = append(parts, fmt.Sprintf("Context: %s (%s)", strings.Join(resolved, ", "), source))
	}
	parts = append(parts, fmt.Sprintf("%d nodes found", len(nodes)))
	header := strings.Join(parts, " | ") + "\n\n"

	return header + g.subgraphToText(nodes, edges, budget, start)
}

// GetNode returns full details for the first node matching label. Resolution
// goes through findNode (exact > prefix > substring, label OR id) so it agrees
// with GetNeighbors/GetCommunity instead of using a divergent substring scan.
// The output also lists the node's relation vocabulary and, when the label was
// ambiguous, hints to disambiguate via the exact node ID.
func (g *Graph) GetNode(label string) string {
	matches := g.findNode(strings.ToLower(label))
	if len(matches) == 0 {
		return fmt.Sprintf("No node matching '%s' found.", strings.ToLower(label))
	}

	id := matches[0]
	d := g.Nodes[id]

	community := ""
	if d.Community != nil {
		community = fmt.Sprintf("%d", *d.Community)
	}

	lines := []string{
		"Node: " + sanitize(g.labelOf(id)),
		"  ID: " + sanitize(id),
		"  Source: " + sanitize(d.SourceFile) + " " + sanitize(d.SourceLocation),
		"  Type: " + sanitize(d.FileType),
		"  Community: " + sanitize(community),
		fmt.Sprintf("  Degree: %d", g.Degree(id)),
	}

	if rels := g.relationsOf(id); len(rels) > 0 {
		lines = append(lines, "  Relations: "+sanitize(strings.Join(rels, ", ")))
	}

	if hint := g.ambiguityHint(label, matches); hint != "" {
		lines = append(lines, hint)
	}

	return strings.Join(lines, "\n")
}

// GetNeighborsPage returns one bounded page of the direct successors then
// predecessors of the first matching node, optionally filtered by relation
// substring. A relation_filter that matches none of the node's actual relations
// is reported as an error listing the valid relations, instead of silently
// returning an empty list (which reads as "no neighbours" and produces false
// negatives such as "this func has no callers"). When the label was ambiguous
// it also hints to use the node ID.
//
// There is deliberately no unpaged variant: a hub node's neighbourhood is
// unbounded, and dumping all of it is what the paging exists to prevent.
func (g *Graph) GetNeighborsPage(label, relationFilter string, page, perPage int) string {
	matches := g.findNode(strings.ToLower(label))
	if len(matches) == 0 {
		return fmt.Sprintf("No node matching '%s' found.", strings.ToLower(label))
	}

	nid := matches[0]
	rf := strings.ToLower(relationFilter)

	// Fail loud on a filter that cannot match any of this node's relations.
	if rf != "" {
		rels := g.relationsOf(nid)
		matchesAny := false
		for _, rel := range rels {
			if strings.Contains(strings.ToLower(rel), rf) {
				matchesAny = true

				break
			}
		}

		if !matchesAny {
			valid := "(none — this node has no edges)"
			if len(rels) > 0 {
				valid = strings.Join(rels, ", ")
			}

			return fmt.Sprintf(
				"No relation matching '%s' on node %s. Valid relations: %s",
				sanitize(relationFilter), sanitize(g.labelOf(nid)), sanitize(valid))
		}
	}

	var entries []string

	for _, r := range g.Successors(nid) {
		rel := r.edge.Relation
		if rf != "" && !strings.Contains(strings.ToLower(rel), rf) {
			continue
		}
		entries = append(entries, fmt.Sprintf("  --> %s [%s] [%s]",
			sanitize(g.labelOf(r.other)), sanitize(rel), sanitize(r.edge.Confidence)))
	}

	for _, r := range g.Predecessors(nid) {
		rel := r.edge.Relation
		if rf != "" && !strings.Contains(strings.ToLower(rel), rf) {
			continue
		}
		entries = append(entries, fmt.Sprintf("  <-- %s [%s] [%s]",
			sanitize(g.labelOf(r.other)), sanitize(rel), sanitize(r.edge.Confidence)))
	}

	// pageBounds normalizes page and per_page, so the header always reports the
	// values actually applied rather than what the caller asked for.
	page, perPage, start, end := pageBounds(len(entries), page, perPage)
	lines := []string{fmt.Sprintf("Neighbors of %s (%d total, page %d, per_page %d, has_more=%t):",
		sanitize(g.labelOf(nid)), len(entries), page, perPage, end < len(entries))}
	lines = append(lines, entries[start:end]...)

	if hint := g.ambiguityHint(label, matches); hint != "" {
		lines = append(lines, hint)
	}

	return strings.Join(lines, "\n")
}

// GetCommunityPage returns one bounded page of the nodes in a community
// (mirrors _tool_get_community). Like GetNeighborsPage it has no unpaged
// variant: a community can hold most of the graph.
func (g *Graph) GetCommunityPage(cid, page, perPage int) string {
	nodes := g.Community(cid)
	if len(nodes) == 0 {
		return fmt.Sprintf("Community %d not found.", cid)
	}

	page, perPage, start, end := pageBounds(len(nodes), page, perPage)
	lines := []string{fmt.Sprintf("Community %d (%d nodes, page %d, per_page %d, has_more=%t):",
		cid, len(nodes), page, perPage, end < len(nodes))}
	for _, n := range nodes[start:end] {
		d := g.Nodes[n]
		lines = append(lines, fmt.Sprintf("  %s [%s]", sanitize(g.labelOf(n)), sanitize(d.SourceFile)))
	}

	return strings.Join(lines, "\n")
}

// GodNodes returns the most-connected real entities (mirrors _tool_god_nodes +
// graphify.analyze.god_nodes, excluding file/concept/json-key nodes).
func (g *Graph) GodNodes(topN int) string {
	if topN <= 0 {
		topN = 10
	}
	if topN > 50 {
		topN = 50
	}

	lines := []string{"God nodes (most connected):"}
	i := 0
	for _, id := range g.nodesByDegreeDesc() {
		if g.isFileNode(id) || g.isConceptNode(id) || g.isJSONKeyNode(id) {
			continue
		}
		i++
		lines = append(lines, fmt.Sprintf("  %d. %s - %d edges", i, g.labelOf(id), g.Degree(id)))
		if i >= topN {
			break
		}
	}

	return strings.Join(lines, "\n")
}

// GraphStats returns node/edge/community counts and confidence breakdown
// (mirrors _tool_graph_stats).
func (g *Graph) GraphStats() string {
	extracted, inferred, ambiguous, total := 0, 0, 0, 0
	for _, refs := range g.out {
		for _, r := range refs {
			total++
			switch r.edge.Confidence {
			case "INFERRED":
				inferred++
			case "AMBIGUOUS":
				ambiguous++
			default: // "" or "EXTRACTED"
				extracted++
			}
		}
	}

	denom := total
	if denom == 0 {
		denom = 1
	}

	pct := func(n int) int { return int(math.Round(float64(n) / float64(denom) * 100)) }

	return fmt.Sprintf(
		"Nodes: %d\nEdges: %d\nCommunities: %d\nEXTRACTED: %d%%\nINFERRED: %d%%\nAMBIGUOUS: %d%%\n",
		g.NumNodes(), g.NumEdges(), g.NumCommunities(),
		pct(extracted), pct(inferred), pct(ambiguous),
	)
}

// ShortestPath finds the shortest path between two concepts over an undirected
// view (mirrors _tool_shortest_path, including the same-node guard, ambiguity
// warnings and max-hops check).
func (g *Graph) ShortestPath(source, target string, maxHops int) string {
	if maxHops <= 0 {
		maxHops = 8
	}
	if maxHops > 12 {
		maxHops = 12
	}

	srcScored := g.scoreNodes(splitLower(source))
	tgtScored := g.scoreNodes(splitLower(target))
	if len(srcScored) == 0 {
		return fmt.Sprintf("No node matching source '%s' found.", source)
	}
	if len(tgtScored) == 0 {
		return fmt.Sprintf("No node matching target '%s' found.", target)
	}

	srcID, tgtID := srcScored[0].id, tgtScored[0].id
	if srcID == tgtID {
		return fmt.Sprintf(
			"'%s' and '%s' both resolved to the same node '%s'. Use a more specific label or the exact node ID.",
			source, target, srcID)
	}

	var warnings []string
	for _, pair := range []struct {
		name   string
		scored []scoredNode
	}{{"source", srcScored}, {"target", tgtScored}} {
		if len(pair.scored) >= 2 {
			top, runner := pair.scored[0].score, pair.scored[1].score
			if top > 0 && (top-runner)/top < 0.10 {
				warnings = append(warnings, fmt.Sprintf(
					"warning: %s match was ambiguous (top score %g, runner-up %g)",
					pair.name, top, runner))
			}
		}
	}

	path, ok := g.undirectedShortestPath(srcID, tgtID)
	if !ok {
		return fmt.Sprintf("No path found between '%s' and '%s'.", g.labelOf(srcID), g.labelOf(tgtID))
	}

	hops := len(path) - 1
	if hops > maxHops {
		return fmt.Sprintf("Path exceeds max_hops=%d (%d hops found).", maxHops, hops)
	}

	var segments []string
	for i, hop := range path {
		if i == 0 {
			segments = append(segments, g.labelOf(hop.node))

			continue
		}

		rel, conf := "", ""
		if hop.edge != nil {
			rel, conf = hop.edge.Relation, hop.edge.Confidence
		}
		confStr := ""
		if conf != "" {
			confStr = " [" + conf + "]"
		}

		if hop.forward {
			segments = append(segments, fmt.Sprintf("--%s%s--> %s", rel, confStr, g.labelOf(hop.node)))
		} else {
			segments = append(segments, fmt.Sprintf("<--%s%s-- %s", rel, confStr, g.labelOf(hop.node)))
		}
	}

	prefix := ""
	if len(warnings) > 0 {
		prefix = strings.Join(warnings, "\n") + "\n"
	}

	return prefix + fmt.Sprintf("Shortest path (%d hops):\n  %s", hops, strings.Join(segments, " "))
}

// pathHop is one node on a shortest path, plus the edge traversed to reach it and
// whether that edge was followed along its own direction. Carrying the edge here
// keeps ShortestPath from re-scanning a node's adjacency once per hop.
type pathHop struct {
	node    string
	edge    *Edge
	forward bool
}

// undirectedShortestPath is BFS over the undirected view (successors +
// predecessors), returning the hops of the path and whether one exists.
func (g *Graph) undirectedShortestPath(src, tgt string) ([]pathHop, bool) {
	if src == tgt {
		return []pathHop{{node: src}}, true
	}

	// step records how a node was first reached, so the path can be rebuilt with
	// its edges instead of looking them up again.
	type step struct {
		from    string
		edge    *Edge
		forward bool
	}

	prev := map[string]step{src: {}}
	queue := []string{src}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if cur == tgt {
			// Reconstruct.
			var path []pathHop
			for n := tgt; ; {
				s := prev[n]
				path = append([]pathHop{{node: n, edge: s.edge, forward: s.forward}}, path...)
				if n == src {
					break
				}
				n = s.from
			}

			return path, true
		}

		for _, nb := range g.undirectedEdges(cur) {
			if _, seen := prev[nb.other]; !seen {
				prev[nb.other] = step{from: cur, edge: nb.edge, forward: nb.forward}
				queue = append(queue, nb.other)
			}
		}
	}

	return nil, false
}

// undirectedEdge is a neighbour reachable in either direction: the linking edge
// plus whether it points away from the queried node.
type undirectedEdge struct {
	other   string
	edge    *Edge
	forward bool
}

// undirectedEdges returns successor then predecessor neighbours in a stable
// order, keeping only the first edge per neighbour (matching the single-edge
// view graphify.build.edge_data exposes).
func (g *Graph) undirectedEdges(id string) []undirectedEdge {
	seen := map[string]bool{}
	var out []undirectedEdge
	for _, r := range g.out[id] {
		if !seen[r.other] {
			seen[r.other] = true
			out = append(out, undirectedEdge{other: r.other, edge: r.edge, forward: true})
		}
	}
	for _, r := range g.in[id] {
		if !seen[r.other] {
			seen[r.other] = true
			out = append(out, undirectedEdge{other: r.other, edge: r.edge})
		}
	}

	return out
}

// ---- god-node classification (mirrors graphify.analyze helpers) -------------

func (g *Graph) isFileNode(id string) bool {
	d := g.Nodes[id]
	if d.Label == "" {
		return false
	}

	if d.SourceFile != "" && d.Label == filepath.Base(d.SourceFile) {
		return true
	}

	if strings.HasPrefix(d.Label, ".") && strings.HasSuffix(d.Label, "()") {
		return true
	}

	if strings.HasSuffix(d.Label, "()") && g.Degree(id) <= 1 {
		return true
	}

	return false
}

func (g *Graph) isConceptNode(id string) bool {
	src := g.Nodes[id].SourceFile
	if src == "" {
		return true
	}

	base := src
	if i := strings.LastIndex(src, "/"); i >= 0 {
		base = src[i+1:]
	}

	return !strings.Contains(base, ".")
}

var jsonNoiseLabels = map[string]bool{
	"start": true, "end": true, "name": true, "id": true, "type": true, jsonPropertiesKey: true,
	"value": true, "key": true, "data": true, "items": true, "title": true, "description": true,
	"version": true, "dependencies": true, "devdependencies": true, "peerdependencies": true,
	"optionaldependencies": true, "bundleddependencies": true, "bundledependencies": true,
}

func (g *Graph) isJSONKeyNode(id string) bool {
	d := g.Nodes[id]
	if !strings.HasSuffix(strings.ToLower(d.SourceFile), ".json") {
		return false
	}

	return jsonNoiseLabels[strings.ToLower(strings.TrimSpace(d.Label))]
}

// relationsOf returns the distinct relation types on a node's edges (both
// directions), sorted for deterministic output. This is the vocabulary a caller
// may pass to relation_filter, so it is surfaced by GetNode and in the
// GetNeighbors "no relation matching" error.
func (g *Graph) relationsOf(id string) []string {
	seen := map[string]bool{}
	for _, r := range g.out[id] {
		if r.edge.Relation != "" {
			seen[r.edge.Relation] = true
		}
	}
	for _, r := range g.in[id] {
		if r.edge.Relation != "" {
			seen[r.edge.Relation] = true
		}
	}

	rels := make([]string, 0, len(seen))
	for rel := range seen {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	return rels
}

// ambiguityHint returns a one-line disambiguation hint when the raw label
// resolved to more than one candidate node, echoing the chosen node's ID so the
// caller can re-query precisely. Empty string when the match was unambiguous.
// This mirrors the ambiguity warning ShortestPath already emits, so all three
// resolving tools behave consistently.
func (g *Graph) ambiguityHint(label string, matches []string) string {
	if len(matches) < 2 {
		return ""
	}

	return fmt.Sprintf(
		"  Note: '%s' matched %d nodes; showing '%s'. Re-query by ID for a specific one.",
		sanitize(label), len(matches), sanitize(matches[0]))
}

// ---- small helpers ----------------------------------------------------------

// splitLower splits on whitespace and lowercases each token (mirrors
// [t.lower() for t in s.split()] used by shortest_path).
func splitLower(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.ToLower(f)
	}

	return out
}

func pageBounds(total, page, perPage int) (int, int, int, int) {
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 200 {
		perPage = 200
	}
	start := total
	if page-1 <= total/perPage {
		start = (page - 1) * perPage
	}
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return page, perPage, start, end
}

// formatLabelList renders a Python-style list repr: ['a', 'b'].
func formatLabelList(labels []string) string {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = "'" + l + "'"
	}

	return "[" + strings.Join(quoted, ", ") + "]"
}

// GodNodesList returns the top god nodes as structured data, for docgen's
// overview generation (parallels GodNodes text rendering).
func (g *Graph) GodNodesList(topN int) []GodNode {
	if topN <= 0 {
		topN = 10
	}

	var out []GodNode
	for _, id := range g.nodesByDegreeDesc() {
		if g.isFileNode(id) || g.isConceptNode(id) || g.isJSONKeyNode(id) {
			continue
		}
		out = append(out, GodNode{ID: id, Label: g.labelOf(id), Degree: g.Degree(id)})
		if len(out) >= topN {
			break
		}
	}

	return out
}

// GodNode is a structured god-node entry.
type GodNode struct {
	ID     string
	Label  string
	Degree int
}

// CommunityIDs returns community ids sorted ascending, for docgen iteration.
func (g *Graph) CommunityIDs() []int {
	ids := make([]int, 0, len(g.communities))
	for id := range g.communities {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	return ids
}
