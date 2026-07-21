// Package graphquery is a native Go implementation of the graphify graph query
// tools (query_graph, get_node, get_neighbors, get_community, god_nodes,
// graph_stats, shortest_path). It reads a graphify graph.json directly and
// answers queries in-process, so krabby no longer needs to spawn a
// `python -m graphify.serve` MCP server for every graph. The python graphify
// CLI is still used for building/merging graphs.
//
// The algorithms here are a faithful port of graphify's serve.py: identical
// scoring/IDF, BFS/DFS hub thresholds, seed selection, context filters, god-node
// filtering, shortest path over an undirected view, and label sanitisation.
package graphquery

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// maxGraphFileBytes caps graph.json size to avoid memory bombs (mirrors
// graphify.security.check_graph_file_size_cap default).
const maxGraphFileBytes = 512 * 1024 * 1024

// Node is one graph node. Only fields the query tools read are modeled; the
// rest of the JSON object is ignored on decode.
type Node struct {
	ID             string
	Label          string
	NormLabel      string
	SourceFile     string
	SourceLocation string
	FileType       string
	Community      *int
}

// Edge is one directed edge (source -> target). graphify stores graphs as
// undirected in the file but serve.py forces directed=true on load, so we treat
// every edge as source->target here.
type Edge struct {
	Source     string
	Target     string
	Relation   string
	Confidence string
	Context    string
}

// Graph is an in-memory directed view of a graphify graph.json.
type Graph struct {
	Nodes    map[string]*Node
	nodeList []string // insertion order (matches JSON node order for stable output)

	out map[string][]edgeRef // successors: source -> edges
	in  map[string][]edgeRef // predecessors: target -> edges

	// degree is total (in+out) degree per node, matching networkx DiGraph.degree.
	degree map[string]int

	communities map[int][]string // community id -> node ids (insertion order)

	// idfCache memoises IDF weights per query term (see scoring.go). Rebuilt with
	// the graph, so it auto-invalidates on reload.
	idfCache map[string]float64
}

// edgeRef points at one edge and the node on the other end.
type edgeRef struct {
	other string
	edge  *Edge
}

// rawGraph mirrors the graph.json layout. graphify writes edges under "links"
// (networkx >= 3.2) but older graphs use "edges"; we accept both.
type rawGraph struct {
	Nodes []json.RawMessage `json:"nodes"`
	Links []json.RawMessage `json:"links"`
	Edges []json.RawMessage `json:"edges"`
}

type rawNode struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	NormLabel      string `json:"norm_label"`
	SourceFile     string `json:"source_file"`
	SourceLocation string `json:"source_location"`
	FileType       string `json:"file_type"`
	Community      *int   `json:"community"`
}

type rawEdge struct {
	Source     string `json:"source"`
	Target     string `json:"target"`
	Relation   string `json:"relation"`
	Confidence string `json:"confidence"`
	Context    string `json:"context"`
}

// Load reads and parses a graphify graph.json into an in-memory Graph.
func Load(path string) (*Graph, error) {
	if !strings.HasSuffix(path, ".json") {
		return nil, fmt.Errorf("graph path must be a .json file, got: %q", path)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat graph %s; %w", path, err)
	}

	if fi.Size() > maxGraphFileBytes {
		return nil, fmt.Errorf("graph file %s is %d bytes, exceeds %d-byte cap", path, fi.Size(), maxGraphFileBytes)
	}

	b, err := os.ReadFile(path) //nolint:gosec // path resolved by manager from tracked repos
	if err != nil {
		return nil, fmt.Errorf("read graph %s; %w", path, err)
	}

	var raw rawGraph
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("graph.json is corrupted (%w); re-run the build to rebuild", err)
	}

	return fromRaw(&raw)
}

func fromRaw(raw *rawGraph) (*Graph, error) {
	g := &Graph{
		Nodes:       make(map[string]*Node, len(raw.Nodes)),
		nodeList:    make([]string, 0, len(raw.Nodes)),
		out:         map[string][]edgeRef{},
		in:          map[string][]edgeRef{},
		degree:      map[string]int{},
		communities: map[int][]string{},
		idfCache:    map[string]float64{},
	}

	for _, rn := range raw.Nodes {
		var n rawNode
		if err := json.Unmarshal(rn, &n); err != nil {
			return nil, fmt.Errorf("decode node; %w", err)
		}

		if n.ID == "" {
			continue
		}

		node := &Node{
			ID:             n.ID,
			Label:          n.Label,
			NormLabel:      n.NormLabel,
			SourceFile:     n.SourceFile,
			SourceLocation: n.SourceLocation,
			FileType:       n.FileType,
			Community:      n.Community,
		}
		g.Nodes[n.ID] = node
		g.nodeList = append(g.nodeList, n.ID)

		if n.Community != nil {
			g.communities[*n.Community] = append(g.communities[*n.Community], n.ID)
		}
	}

	links := raw.Links
	if len(links) == 0 {
		links = raw.Edges
	}

	for _, re := range links {
		var e rawEdge
		if err := json.Unmarshal(re, &e); err != nil {
			return nil, fmt.Errorf("decode edge; %w", err)
		}

		// Skip dangling edges whose endpoints are missing (networkx would raise).
		if _, ok := g.Nodes[e.Source]; !ok {
			continue
		}

		if _, ok := g.Nodes[e.Target]; !ok {
			continue
		}

		edge := &Edge{
			Source:     e.Source,
			Target:     e.Target,
			Relation:   e.Relation,
			Confidence: e.Confidence,
			Context:    e.Context,
		}
		g.out[e.Source] = append(g.out[e.Source], edgeRef{other: e.Target, edge: edge})
		g.in[e.Target] = append(g.in[e.Target], edgeRef{other: e.Source, edge: edge})
		g.degree[e.Source]++
		g.degree[e.Target]++
	}

	return g, nil
}

// NumNodes returns the node count.
func (g *Graph) NumNodes() int { return len(g.nodeList) }

// NumEdges returns the directed edge count.
func (g *Graph) NumEdges() int {
	total := 0
	for _, refs := range g.out {
		total += len(refs)
	}

	return total
}

// Degree returns the total (in+out) degree of a node, matching networkx.
func (g *Graph) Degree(id string) int { return g.degree[id] }

// Successors returns outgoing edges from id (source -> id -> other).
func (g *Graph) Successors(id string) []edgeRef { return g.out[id] }

// Predecessors returns incoming edges to id (other -> id).
func (g *Graph) Predecessors(id string) []edgeRef { return g.in[id] }

// Neighbors returns successor node ids (networkx DiGraph.neighbors == successors),
// used by BFS/DFS which mirror the python traversal over successors.
func (g *Graph) Neighbors(id string) []string {
	refs := g.out[id]
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.other)
	}

	return out
}

// Edge returns the first edge attributes for (u, v) in either direction, matching
// graphify.build.edge_data used across the tools. ok is false when no edge exists.
func (g *Graph) Edge(u, v string) (*Edge, bool) {
	for _, r := range g.out[u] {
		if r.other == v {
			return r.edge, true
		}
	}

	return nil, false
}

// HasEdge reports whether a directed edge u -> v exists.
func (g *Graph) HasEdge(u, v string) bool {
	_, ok := g.Edge(u, v)

	return ok
}

// Community returns the node ids in a community, in insertion order.
func (g *Graph) Community(id int) []string { return g.communities[id] }

// NumCommunities returns the number of distinct communities.
func (g *Graph) NumCommunities() int { return len(g.communities) }

// nodesByDegreeDesc returns node ids sorted by degree descending. Ties keep
// insertion order (stable) so output is deterministic.
func (g *Graph) nodesByDegreeDesc() []string {
	ids := make([]string, len(g.nodeList))
	copy(ids, g.nodeList)
	sort.SliceStable(ids, func(i, j int) bool {
		return g.degree[ids[i]] > g.degree[ids[j]]
	})

	return ids
}
