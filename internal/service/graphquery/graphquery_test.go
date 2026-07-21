package graphquery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGraph writes a graph.json fixture and returns its path.
func writeGraph(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "graph.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

// smallGraph: A -> B (call), A -> C (field), B -> C (call). A has label "Service".
const smallGraph = `{
  "directed": false, "multigraph": false, "nodes": [
    {"id":"a","label":"Service","norm_label":"service","source_file":"svc.go","source_location":"L1","file_type":"code","community":0},
    {"id":"b","label":"handleRequest","norm_label":"handlerequest","source_file":"svc.go","source_location":"L2","file_type":"code","community":0},
    {"id":"c","label":"Config","norm_label":"config","source_file":"cfg.go","source_location":"L3","file_type":"code","community":1}
  ], "links": [
    {"source":"a","target":"b","relation":"calls","confidence":"EXTRACTED","context":"call"},
    {"source":"a","target":"c","relation":"references","confidence":"INFERRED","context":"field"},
    {"source":"b","target":"c","relation":"calls","confidence":"EXTRACTED","context":"call"}
  ]
}`

func loadSmall(t *testing.T) *Graph {
	t.Helper()
	g, err := Load(writeGraph(t, smallGraph))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return g
}

func TestLoadCounts(t *testing.T) {
	g := loadSmall(t)
	if g.NumNodes() != 3 {
		t.Errorf("nodes = %d, want 3", g.NumNodes())
	}
	if g.NumEdges() != 3 {
		t.Errorf("edges = %d, want 3", g.NumEdges())
	}
	if g.NumCommunities() != 2 {
		t.Errorf("communities = %d, want 2", g.NumCommunities())
	}
	// Degree is in+out: c has 2 incoming, a has 2 outgoing, b has 1 in + 1 out.
	if d := g.Degree("c"); d != 2 {
		t.Errorf("degree(c) = %d, want 2", d)
	}
	if d := g.Degree("b"); d != 2 {
		t.Errorf("degree(b) = %d, want 2", d)
	}
}

func TestGraphStats(t *testing.T) {
	g := loadSmall(t)
	got := g.GraphStats()
	want := "Nodes: 3\nEdges: 3\nCommunities: 2\nEXTRACTED: 67%\nINFERRED: 33%\nAMBIGUOUS: 0%\n"
	if got != want {
		t.Errorf("stats mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestGetNode(t *testing.T) {
	g := loadSmall(t)
	got := g.GetNode("service")
	for _, want := range []string{"Node: Service", "ID: a", "Source: svc.go L1", "Degree: 2", "Community: 0"} {
		if !strings.Contains(got, want) {
			t.Errorf("get_node output missing %q in:\n%s", want, got)
		}
	}

	if miss := g.GetNode("nope"); !strings.Contains(miss, "No node matching") {
		t.Errorf("expected miss message, got %q", miss)
	}
}

func TestGetNeighbors(t *testing.T) {
	g := loadSmall(t)
	got := g.GetNeighbors("Service", "")
	if !strings.Contains(got, "--> handleRequest [calls] [EXTRACTED]") {
		t.Errorf("missing successor edge:\n%s", got)
	}
	if !strings.Contains(got, "--> Config [references] [INFERRED]") {
		t.Errorf("missing successor edge:\n%s", got)
	}

	// relation filter keeps only 'calls'
	filtered := g.GetNeighbors("Service", "call")
	if strings.Contains(filtered, "references") {
		t.Errorf("relation filter leaked references:\n%s", filtered)
	}
}

func TestGetCommunity(t *testing.T) {
	g := loadSmall(t)
	got := g.GetCommunity(0)
	if !strings.Contains(got, "Community 0 (2 nodes):") {
		t.Errorf("community header wrong:\n%s", got)
	}
	if miss := g.GetCommunity(99); !strings.Contains(miss, "not found") {
		t.Errorf("expected not found, got %q", miss)
	}
}

func TestQueryGraph(t *testing.T) {
	g := loadSmall(t)
	got := g.QueryGraph("Service", QueryGraphOpts{})
	if !strings.HasPrefix(got, "Traversal: BFS depth=3 | Start: ['Service']") {
		t.Errorf("query header wrong:\n%s", got)
	}
	if !strings.Contains(got, "NODE Service [src=svc.go loc=L1 community=0]") {
		t.Errorf("missing seed node line:\n%s", got)
	}

	// context_filter=['call'] should drop the field edge to Config from A.
	filtered := g.QueryGraph("Service", QueryGraphOpts{ContextFilter: []string{"call"}})
	if !strings.Contains(filtered, "Context: call (explicit)") {
		t.Errorf("expected explicit context header:\n%s", filtered)
	}

	if none := g.QueryGraph("zzzznotfound", QueryGraphOpts{}); none != "No matching nodes found." {
		t.Errorf("expected no-match message, got %q", none)
	}
}

func TestShortestPath(t *testing.T) {
	g := loadSmall(t)
	got := g.ShortestPath("Service", "Config", 8)
	if !strings.Contains(got, "Shortest path (1 hops):") {
		t.Errorf("expected 1-hop path, got:\n%s", got)
	}
	if !strings.Contains(got, "Service") || !strings.Contains(got, "Config") {
		t.Errorf("path endpoints missing:\n%s", got)
	}

	// same node guard
	same := g.ShortestPath("Service", "Service", 8)
	if !strings.Contains(same, "both resolved to the same node") {
		t.Errorf("expected same-node guard, got %q", same)
	}
}

func TestGodNodes(t *testing.T) {
	g := loadSmall(t)
	got := g.GodNodes(10)
	if !strings.HasPrefix(got, "God nodes (most connected):") {
		t.Errorf("god header wrong:\n%s", got)
	}
	// Service (a) should appear; file-name nodes would be excluded but there are none here.
	if !strings.Contains(got, "Service") {
		t.Errorf("expected Service in god nodes:\n%s", got)
	}
}

func TestEngineCallAndReload(t *testing.T) {
	p := writeGraph(t, smallGraph)
	e := NewEngine()

	out, err := e.Call(p, "graph_stats", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(out, "Nodes: 3") {
		t.Errorf("engine stats wrong:\n%s", out)
	}

	if _, err := e.Call(p, "bogus_tool", nil); err == nil {
		t.Error("expected error for unknown tool")
	}

	// query_graph via engine with args map (mirrors MCP dispatch).
	q, err := e.Call(p, "query_graph", map[string]any{"question": "Service", "depth": float64(2)})
	if err != nil {
		t.Fatalf("query call: %v", err)
	}
	if !strings.Contains(q, "Traversal: BFS depth=2") {
		t.Errorf("depth arg not honoured:\n%s", q)
	}
}

func TestContextFilterAliases(t *testing.T) {
	got := normalizeContextFilters([]string{"params", "arg", "returns", "unknownctx"})
	want := map[string]bool{"parameter_type": true, "return_type": true, "unknownctx": true}
	if len(got) != len(want) {
		t.Fatalf("normalize got %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected normalized filter %q", g)
		}
	}
}

func TestQueryTermsSearchable(t *testing.T) {
	// Short ASCII words (<=2) are dropped; longer ones kept.
	terms := queryTerms("to be FooBarService")
	joined := strings.Join(terms, ",")
	if strings.Contains(joined, "to") || strings.Contains(joined, "be") {
		t.Errorf("short terms not dropped: %v", terms)
	}
	if !strings.Contains(joined, "foobarservice") {
		t.Errorf("identifier term missing: %v", terms)
	}
}
