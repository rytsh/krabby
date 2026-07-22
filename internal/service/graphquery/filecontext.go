package graphquery

import (
	"fmt"
	"sort"
	"strings"
)

// FileContext renders the graph neighborhood for a single source file as compact
// text suitable for embedding in a documentation LLM prompt. It lists every node
// whose source_file equals sourceFile, then the edges those nodes participate in
// (outgoing first, then incoming from other files), so the model sees both the
// symbols defined in the file and how they connect to the rest of the codebase.
//
// maxLines caps the output; 0 uses a sane default.
func (g *Graph) FileContext(sourceFile string, maxLines int) string {
	if maxLines <= 0 {
		maxLines = 200
	}

	// Collect nodes defined in this file, in stable order.
	var fileNodes []string
	inFile := map[string]bool{}
	for _, id := range g.nodeList {
		if g.Nodes[id].SourceFile == sourceFile {
			fileNodes = append(fileNodes, id)
			inFile[id] = true
		}
	}

	if len(fileNodes) == 0 {
		return ""
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Symbols defined in %s:", sanitize(sourceFile)))
	for _, id := range fileNodes {
		lines = append(lines, "  "+g.nodeSummary(id))
	}

	// Outgoing edges from file nodes (what this file uses/references).
	var out []string
	seen := map[string]bool{}
	for _, id := range fileNodes {
		for _, r := range g.Successors(id) {
			line := fmt.Sprintf("  %s --%s--> %s",
				sanitize(g.labelOf(id)), sanitize(r.edge.Relation), sanitize(g.labelOf(r.other)))
			if !seen[line] {
				seen[line] = true
				out = append(out, line)
			}
		}
	}
	sort.Strings(out)
	if len(out) > 0 {
		lines = append(lines, "", "Relationships (this file -> others):")
		lines = append(lines, out...)
	}

	// Incoming edges from OTHER files (who depends on this file).
	var in []string
	seen = map[string]bool{}
	for _, id := range fileNodes {
		for _, r := range g.Predecessors(id) {
			if inFile[r.other] {
				continue // internal edge already shown above
			}
			line := fmt.Sprintf("  %s --%s--> %s",
				sanitize(g.labelOf(r.other)), sanitize(r.edge.Relation), sanitize(g.labelOf(id)))
			if !seen[line] {
				seen[line] = true
				in = append(in, line)
			}
		}
	}
	sort.Strings(in)
	if len(in) > 0 {
		lines = append(lines, "", "Used by (others -> this file):")
		lines = append(lines, in...)
	}

	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines = append(lines, "... (graph context truncated)")
	}

	return strings.Join(lines, "\n")
}

// nodeSummary is a one-line node description used in file context.
func (g *Graph) nodeSummary(id string) string {
	d := g.Nodes[id]
	community := ""
	if d.Community != nil {
		community = fmt.Sprintf(" community=%d", *d.Community)
	}

	loc := ""
	if d.SourceLocation != "" {
		loc = " " + sanitize(d.SourceLocation)
	}

	return fmt.Sprintf("%s%s%s", sanitize(g.labelOf(id)), loc, community)
}

// CommunityFiles groups the graph's source files by community id. A file is
// attributed to the community that most of its symbol nodes belong to (majority
// vote; ties break toward the lowest community id for determinism). Files whose
// nodes carry no community are collected under the returned "ungrouped" slice.
// The result lets docgen summarize a cluster of related files in one LLM call
// instead of one call per file.
func (g *Graph) CommunityFiles() (byCommunity map[int][]string, ungrouped []string) {
	// For each file, count how many of its nodes fall in each community.
	votes := map[string]map[int]int{} // file -> community -> count
	seenFile := map[string]bool{}

	for _, id := range g.nodeList {
		n := g.Nodes[id]
		if n.SourceFile == "" {
			continue
		}

		file := n.SourceFile
		seenFile[file] = true

		if n.Community == nil {
			continue
		}

		if votes[file] == nil {
			votes[file] = map[int]int{}
		}
		votes[file][*n.Community]++
	}

	byCommunity = map[int][]string{}
	for _, file := range sortedKeys(seenFile) {
		tally := votes[file]
		if len(tally) == 0 {
			ungrouped = append(ungrouped, file)

			continue
		}

		best, bestCount := 0, -1
		for _, cid := range sortedIntKeys(tally) {
			if tally[cid] > bestCount {
				best, bestCount = cid, tally[cid]
			}
		}

		byCommunity[best] = append(byCommunity[best], file)
	}

	return byCommunity, ungrouped
}

// CommunityContext renders the graph neighborhood for a whole community: the
// files it spans, its most-connected symbols and the cross-file relationships
// among its members, as compact text for a documentation LLM prompt. maxLines
// caps the output; 0 uses a sane default.
func (g *Graph) CommunityContext(communityID int, files []string, maxLines int) string {
	if maxLines <= 0 {
		maxLines = 300
	}

	members := g.communities[communityID]
	if len(members) == 0 && len(files) == 0 {
		return ""
	}

	inCommunity := make(map[string]bool, len(members))
	for _, id := range members {
		inCommunity[id] = true
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Community %d spans %d file(s):", communityID, len(files)))
	for _, f := range files {
		lines = append(lines, "  "+sanitize(f))
	}

	// Most-connected symbols in this community (its local core abstractions).
	ranked := make([]string, len(members))
	copy(ranked, members)
	sort.SliceStable(ranked, func(i, j int) bool {
		return g.degree[ranked[i]] > g.degree[ranked[j]]
	})

	limit := min(15, len(ranked))
	if limit > 0 {
		lines = append(lines, "", "Key symbols (most connected):")
		for _, id := range ranked[:limit] {
			lines = append(lines, "  "+g.nodeSummary(id))
		}
	}

	// Cross-file relationships that reach outside the community (how this
	// cluster connects to the rest of the system).
	var external []string
	seen := map[string]bool{}
	for _, id := range members {
		for _, r := range g.Successors(id) {
			if inCommunity[r.other] {
				continue
			}
			line := fmt.Sprintf("  %s --%s--> %s",
				sanitize(g.labelOf(id)), sanitize(r.edge.Relation), sanitize(g.labelOf(r.other)))
			if !seen[line] {
				seen[line] = true
				external = append(external, line)
			}
		}
	}
	sort.Strings(external)
	if len(external) > 0 {
		lines = append(lines, "", "Outgoing relationships (this cluster -> rest of system):")
		lines = append(lines, external...)
	}

	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines = append(lines, "... (graph context truncated)")
	}

	return strings.Join(lines, "\n")
}

// sortedKeys returns the keys of a string-set in ascending order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}

// sortedIntKeys returns the keys of an int-keyed map in ascending order.
func sortedIntKeys(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)

	return out
}

// OverviewContext renders a high-level summary of the whole graph (stats, god
// nodes, largest communities) for an overview document prompt.
func (g *Graph) OverviewContext(topGodNodes, topCommunities int) string {
	if topGodNodes <= 0 {
		topGodNodes = 15
	}
	if topCommunities <= 0 {
		topCommunities = 10
	}

	var b strings.Builder
	b.WriteString("Graph statistics:\n")
	b.WriteString(g.GraphStats())

	b.WriteString("\nMost connected symbols (core abstractions):\n")
	for i, n := range g.GodNodesList(topGodNodes) {
		fmt.Fprintf(&b, "  %d. %s (%d edges)\n", i+1, sanitize(n.Label), n.Degree)
	}

	// Largest communities by node count.
	ids := g.CommunityIDs()
	sort.SliceStable(ids, func(i, j int) bool {
		return len(g.communities[ids[i]]) > len(g.communities[ids[j]])
	})

	limit := min(topCommunities, len(ids))
	if limit > 0 {
		b.WriteString("\nLargest communities (clusters of related symbols):\n")
		for _, cid := range ids[:limit] {
			members := g.communities[cid]
			sample := make([]string, 0, 5)
			for _, m := range members {
				if len(sample) >= 5 {
					break
				}
				sample = append(sample, sanitize(g.labelOf(m)))
			}
			fmt.Fprintf(&b, "  Community %d (%d nodes): %s\n", cid, len(members), strings.Join(sample, ", "))
		}
	}

	return b.String()
}
