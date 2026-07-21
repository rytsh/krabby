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
