package graphquery

import (
	"fmt"
	"strings"
	"testing"
)

// navGraph models what symbol navigation must handle: one label defined in three
// files, a weaker prefix match, three referring nodes with distinct contexts and
// one outgoing edge, plus the three structural node classes (file, concept,
// json key) that are not definitions.
const navGraph = `{
  "directed": false, "multigraph": false, "nodes": [
    {"id":"pkga.Handle","label":"Handle","norm_label":"handle","source_file":"pkga/a.go","source_location":"L10-L20","file_type":"code","community":0},
    {"id":"pkgb.Handle","label":"Handle","norm_label":"handle","source_file":"pkgb/b.go","source_location":"line 30","file_type":"code","community":1},
    {"id":"pkgc.Handle","label":"Handle","norm_label":"handle","source_file":"pkgc/c.go","source_location":"42:5","file_type":"code"},
    {"id":"pkgd.HandleAux","label":"HandleAux","norm_label":"handleaux","source_file":"pkgd/d.go","source_location":"L5","file_type":"code"},
    {"id":"pkgx.CallerOne","label":"CallerOne","norm_label":"callerone","source_file":"pkgx/caller.go","source_location":"L7","file_type":"code"},
    {"id":"pkgy.CallerTwo","label":"CallerTwo","norm_label":"callertwo","source_file":"pkgy/other.go","source_location":"L99","file_type":"code"},
    {"id":"pkgz.Importer","label":"Importer","norm_label":"importer","source_file":"pkgz/imp.go","source_location":"L3","file_type":"code"},
    {"id":"pkga.utilfile","label":"util.go","norm_label":"util.go","source_file":"pkga/util.go","source_location":"L1","file_type":"code"},
    {"id":"concept.auth","label":"Authentication","norm_label":"authentication","source_file":"","source_location":"","file_type":"concept"},
    {"id":"pkgjson.name","label":"name","norm_label":"name","source_file":"package.json","source_location":"L2","file_type":"json"}
  ], "links": [
    {"source":"pkgx.CallerOne","target":"pkga.Handle","relation":"calls","confidence":"EXTRACTED","context":"call"},
    {"source":"pkgy.CallerTwo","target":"pkga.Handle","relation":"calls","confidence":"AMBIGUOUS","context":"call"},
    {"source":"pkgz.Importer","target":"pkga.Handle","relation":"imports","confidence":"INFERRED","context":"import"},
    {"source":"pkga.Handle","target":"pkgd.HandleAux","relation":"calls","confidence":"EXTRACTED","context":"call"}
  ]
}`

func loadNav(t *testing.T) *Graph {
	t.Helper()
	g, err := Load(writeGraph(t, navGraph))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return g
}

// A symbol defined in three files must yield three definitions: narrowing to one
// hides the ambiguity the caller has to resolve.
func TestDefinitionsReturnsEveryDefiningSite(t *testing.T) {
	t.Parallel()

	got := loadNav(t).Definitions("Handle", 0)
	if got.Total != 3 || len(got.Definitions) != 3 {
		t.Fatalf("Definitions = %d (total %d), want 3: %+v", len(got.Definitions), got.Total, got.Definitions)
	}
	if got.Note != "" {
		t.Errorf("unexpected note on a successful lookup: %s", got.Note)
	}

	type site struct {
		id      string
		path    string
		line    int
		endLine int
	}
	want := []site{
		{"pkga.Handle", "pkga/a.go", 10, 20},
		{"pkgb.Handle", "pkgb/b.go", 30, 0},
		{"pkgc.Handle", "pkgc/c.go", 42, 0},
	}
	for i, w := range want {
		d := got.Definitions[i]
		if d.ID != w.id || d.Path != w.path || d.Line != w.line || d.EndLine != w.endLine {
			t.Errorf("definition %d = %+v, want %+v", i, d, w)
		}
	}

	if got.Definitions[0].Degree != 4 {
		t.Errorf("degree = %d, want 4", got.Definitions[0].Degree)
	}
	if got.Definitions[0].Community == nil || *got.Definitions[0].Community != 0 {
		t.Errorf("community = %v, want 0", got.Definitions[0].Community)
	}
}

// The weaker match tier must be reported, not mixed into the results: an exact
// hit stays exact and the fuzzy alternative stays visible.
func TestDefinitionsReportsWeakerTierAsCandidates(t *testing.T) {
	t.Parallel()

	got := loadNav(t).Definitions("Handle", 0)
	for _, d := range got.Definitions {
		if d.ID == "pkgd.HandleAux" {
			t.Fatalf("prefix match leaked into exact-tier definitions: %+v", got.Definitions)
		}
	}
	if len(got.Candidates) != 1 || !strings.Contains(got.Candidates[0], "pkgd.HandleAux") {
		t.Fatalf("candidates = %v, want the HandleAux prefix match", got.Candidates)
	}
}

// File, concept and JSON-key nodes are structure, not definitions. Excluding them
// silently would look like "symbol not found", so the exclusion is reported.
func TestDefinitionsExcludeStructuralNodesAndSaySo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		symbol string
		kind   string
	}{
		{"file node", "util", kindFile},
		{"concept node", "Authentication", kindConcept},
		{"json key node", "name", kindJSONKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := loadNav(t).Definitions(tc.symbol, 0)
			if len(got.Definitions) != 0 || got.Total != 0 {
				t.Fatalf("%s returned definitions: %+v", tc.symbol, got.Definitions)
			}
			if !strings.Contains(got.Note, tc.kind) {
				t.Errorf("note does not name the excluded kind %q: %s", tc.kind, got.Note)
			}
			if !strings.Contains(got.Note, "search_code") {
				t.Errorf("note lacks the next action: %s", got.Note)
			}
		})
	}
}

// References are the incoming edges, each carrying the referring node's own path
// and line so the caller can jump there; the symbol's own outgoing edge is not a
// reference to it.
func TestReferencesComeFromIncomingEdgesWithReferrerLocation(t *testing.T) {
	t.Parallel()

	got := loadNav(t).References("Handle", nil, 0, 0)
	if got.Resolved != "pkga.Handle" {
		t.Fatalf("resolved = %q, want pkga.Handle", got.Resolved)
	}
	if got.Total != 3 || len(got.References) != 3 {
		t.Fatalf("references = %d (total %d), want 3: %+v", len(got.References), got.Total, got.References)
	}

	want := []Reference{
		{Label: "CallerOne", ID: "pkgx.CallerOne", Path: "pkgx/caller.go", Line: 7, Relation: "calls", Context: "call", Confidence: "EXTRACTED"},
		{Label: "CallerTwo", ID: "pkgy.CallerTwo", Path: "pkgy/other.go", Line: 99, Relation: "calls", Context: "call", Confidence: "AMBIGUOUS"},
		{Label: "Importer", ID: "pkgz.Importer", Path: "pkgz/imp.go", Line: 3, Relation: "imports", Context: "import", Confidence: "INFERRED"},
	}
	for i, w := range want {
		if got.References[i] != w {
			t.Errorf("reference %d = %+v, want %+v", i, got.References[i], w)
		}
	}

	for _, r := range got.References {
		if r.ID == "pkgd.HandleAux" {
			t.Errorf("outgoing edge reported as a reference: %+v", r)
		}
	}
}

// A contexts filter narrows on the edge context, through the same alias table the
// traversal filters use ("calls" -> "call").
func TestReferencesContextFilterNarrowsThroughAliases(t *testing.T) {
	t.Parallel()

	got := loadNav(t).References("Handle", []string{"calls"}, 0, 0)
	if got.Total != 2 {
		t.Fatalf("total = %d, want 2 call references: %+v", got.Total, got.References)
	}
	for _, r := range got.References {
		if r.Context != ctxCall {
			t.Errorf("reference %+v survived the call filter", r)
		}
	}
	if got.Note != "" {
		t.Errorf("unexpected note: %s", got.Note)
	}
}

// An empty list here reads as "this symbol has no callers". A filter that cannot
// match must therefore return the node's real context vocabulary instead.
func TestReferencesUnmatchedContextFilterReturnsVocabulary(t *testing.T) {
	t.Parallel()

	got := loadNav(t).References("Handle", []string{"field"}, 0, 0)
	if len(got.References) != 0 || got.Total != 0 {
		t.Fatalf("field filter matched references: %+v", got.References)
	}
	for _, want := range []string{"'field'", ctxCall, ctxImport} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("note missing %q: %s", want, got.Note)
		}
	}
}

// A node with no incoming edges is a real outcome, not an error, and must state
// what it means plus how to confirm it.
func TestReferencesWithNoIncomingEdgesExplainsItself(t *testing.T) {
	t.Parallel()

	got := loadNav(t).References("CallerOne", nil, 0, 0)
	if got.Resolved != "pkgx.CallerOne" {
		t.Fatalf("resolved = %q, want pkgx.CallerOne", got.Resolved)
	}
	if len(got.References) != 0 {
		t.Fatalf("references = %+v, want none", got.References)
	}
	for _, want := range []string{"no incoming edges", "search_code"} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("note missing %q: %s", want, got.Note)
		}
	}
}

func TestReferencesPaginationBoundsAndHasMore(t *testing.T) {
	t.Parallel()

	g := loadNav(t)

	first := g.References("Handle", nil, 1, 1)
	if first.Page != 1 || first.PerPage != 1 || len(first.References) != 1 || !first.HasMore {
		t.Fatalf("page 1 = page %d per_page %d len %d has_more %t", first.Page, first.PerPage, len(first.References), first.HasMore)
	}
	if first.References[0].ID != "pkgx.CallerOne" {
		t.Errorf("page 1 entry = %q, want pkgx.CallerOne", first.References[0].ID)
	}

	last := g.References("Handle", nil, 3, 1)
	if len(last.References) != 1 || last.References[0].ID != "pkgz.Importer" || last.HasMore {
		t.Errorf("page 3 = %+v has_more %t", last.References, last.HasMore)
	}

	if capped := g.References("Handle", nil, 1, 5000); capped.PerPage != 200 {
		t.Errorf("per_page = %d, want the 200 hard cap", capped.PerPage)
	}
	if defaulted := g.References("Handle", nil, 0, 0); defaulted.PerPage != 50 {
		t.Errorf("per_page = %d, want the 50 default", defaulted.PerPage)
	}
}

// graphquery must not reach into the text index; an unknown symbol is handed the
// tool that owns regex search, with a ready-to-use word-boundary pattern.
func TestUnknownSymbolYieldsRegexSearchNote(t *testing.T) {
	t.Parallel()

	g := loadNav(t)
	defs := g.Definitions("ZzzNotInGraph", 0)
	refs := g.References("ZzzNotInGraph", nil, 0, 0)

	for name, note := range map[string]string{"definitions": defs.Note, "references": refs.Note} {
		for _, want := range []string{"search_code", "mode='regex'", `pattern='\bZzzNotInGraph\b'`} {
			if !strings.Contains(note, want) {
				t.Errorf("%s note missing %q: %s", name, want, note)
			}
		}
	}

	if len(defs.Definitions) != 0 || defs.Total != 0 {
		t.Errorf("unknown symbol returned definitions: %+v", defs.Definitions)
	}
	if len(refs.References) != 0 || refs.Total != 0 {
		t.Errorf("unknown symbol returned references: %+v", refs.References)
	}
}

func TestParseSourceLocationShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		loc     string
		line    int
		endLine int
	}{
		{"L42", 42, 0},
		{"L42-L60", 42, 60},
		{"L42-60", 42, 60},
		{"line 42", 42, 0},
		{"42:5", 42, 0},
		{"  L42  ", 42, 0},
		{"L60-L42", 60, 0}, // backwards range: keep the start, drop the nonsense end
		{"L0", 0, 0},
		{"unknown", 0, 0},
		{"", 0, 0},
	} {
		t.Run(fmt.Sprintf("%q", tc.loc), func(t *testing.T) {
			t.Parallel()

			line, endLine := parseSourceLocation(tc.loc)
			if line != tc.line || endLine != tc.endLine {
				t.Errorf("parseSourceLocation(%q) = (%d, %d), want (%d, %d)", tc.loc, line, endLine, tc.line, tc.endLine)
			}
		})
	}
}

// hubGraph is root -> hub -> n leaves. Degrees are 1 for root and each leaf and
// n+1 for the hub, which pins the p99-with-floor-50 threshold exactly, and the
// hub sits one hop past a seed so the transit gate is observable.
func hubGraph(n int) string {
	nodes := []string{
		`{"id":"root","label":"Root","norm_label":"root","source_file":"r.go","source_location":"L1"}`,
		`{"id":"hub","label":"Hub","norm_label":"hub","source_file":"h.go","source_location":"L1"}`,
	}
	links := []string{`{"source":"root","target":"hub","relation":"calls","confidence":"EXTRACTED","context":"call"}`}
	for i := range n {
		nodes = append(nodes, fmt.Sprintf(`{"id":"leaf%d","label":"Leaf%d","norm_label":"leaf%d","source_file":"l%d.go","source_location":"L1"}`, i, i, i, i))
		links = append(links, fmt.Sprintf(`{"source":"hub","target":"leaf%d","relation":"calls","confidence":"EXTRACTED","context":"call"}`, i))
	}

	return fmt.Sprintf(`{"directed": false, "multigraph": false, "nodes": [%s], "links": [%s]}`,
		strings.Join(nodes, ","), strings.Join(links, ","))
}

// The hub threshold is derived once at load time instead of allocating and
// sorting the whole degree distribution on every traversal.
func TestHubThresholdComputedAtLoad(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"floor wins for a low p99", hubGraph(40), 50},
		{"p99 wins above the floor", hubGraph(60), 61},
		{"empty graph falls back to the floor", `{"nodes":[],"links":[]}`, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g, err := Load(writeGraph(t, tc.body))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if g.hubThreshold != tc.want {
				t.Fatalf("hubThreshold = %d, want %d", g.hubThreshold, tc.want)
			}
		})
	}
}

// Traversal must read the stored threshold rather than recomputing one: with the
// loaded value (50, above the hub's degree of 41) BFS expands through the hub,
// and lowering the stored value alone stops it.
func TestTraversalUsesStoredHubThreshold(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, hubGraph(40)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	visited, _ := g.bfs([]string{"root"}, 2, nil)
	if len(visited) != 42 {
		t.Fatalf("visited %d nodes, want root, hub and 40 leaves (42)", len(visited))
	}

	g.hubThreshold = 1
	visited, _ = g.bfs([]string{"root"}, 2, nil)
	if len(visited) != 2 {
		t.Fatalf("visited %d nodes, want only root and the unexpanded hub", len(visited))
	}
}

// Rendering uses the edge the traversal followed. Dropping the adjacency after
// traversal makes any Edge() lookup impossible, so intact EDGE lines prove the
// per-edge rescan (quadratic on a high-degree node) is gone.
func TestSubgraphRenderUsesCarriedEdge(t *testing.T) {
	t.Parallel()

	g := loadSmall(t)
	seeds := []string{"a"}
	nodes, edges := g.bfs(seeds, 3, nil)

	before := g.subgraphToText(nodes, edges, 2000, seeds)

	g.out = nil
	after := g.subgraphToText(nodes, edges, 2000, seeds)

	if before != after {
		t.Fatalf("rendering changed without adjacency:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, want := range []string{
		"EDGE Service --calls [EXTRACTED context=call]--> handleRequest",
		"EDGE Service --references [INFERRED context=field]--> Config",
	} {
		if !strings.Contains(after, want) {
			t.Errorf("missing rendered edge %q in:\n%s", want, after)
		}
	}
}

// A reverse hop proves ShortestPath renders the edge the search traversed,
// including its direction, without looking it up again.
func TestShortestPathRendersReverseHopFromCarriedEdge(t *testing.T) {
	t.Parallel()

	got := loadSmall(t).ShortestPath("Config", "Service", 8)
	if !strings.Contains(got, "Config <--references [INFERRED]-- Service") {
		t.Fatalf("reverse hop not rendered from the traversed edge: %s", got)
	}
}

func TestEngineSymbolNavigationSharesTheGraphCache(t *testing.T) {
	t.Parallel()

	p := writeGraph(t, navGraph)
	e := NewEngine(0)

	defs, err := e.Definitions(p, "Handle", 0)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if defs.Total != 3 {
		t.Errorf("total = %d, want 3", defs.Total)
	}

	refs, err := e.References(p, "Handle", []string{"calls"}, 1, 50)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if refs.Total != 2 {
		t.Errorf("total = %d, want 2", refs.Total)
	}

	if _, err := e.Definitions(writeGraph(t, `{`), "Handle", 0); err == nil {
		t.Error("Definitions accepted an unparseable graph")
	}
}

// methodGraph is the shape a real graphify extraction produces and the previous
// test fixture did not: methods are labelled ".Name()" and a receiver type is
// linked to them by a relation that carries no edge context at all.
const methodGraph = `{
  "directed": false, "multigraph": false, "nodes": [
    {"id":"manager.searchcodetext","label":".SearchCodeText()","norm_label":".searchcodetext()","source_file":"internal/service/manager/docs.go","source_location":"L1998","file_type":"code","community":24},
    {"id":"manager.lonely","label":".Lonely()","norm_label":".lonely()","source_file":"internal/service/manager/docs.go","source_location":"L42","file_type":"code"},
    {"id":"manager.type","label":"Manager","norm_label":"manager","source_file":"internal/service/manager/manager.go","source_location":"L30","file_type":"code"},
    {"id":"manager.caller","label":".Handler()","norm_label":".handler()","source_file":"internal/server/server.go","source_location":"L57","file_type":"code"}
  ], "links": [
    {"source":"manager.type","target":"manager.searchcodetext","relation":"method","confidence":"EXTRACTED"},
    {"source":"manager.caller","target":"manager.searchcodetext","relation":"calls","confidence":"EXTRACTED","context":"call"},
    {"source":"manager.type","target":"manager.lonely","relation":"method","confidence":"EXTRACTED"}
  ]
}`

// TestDefinitionsIncludesMethods is the regression test for a classifier reused
// from the wrong place. god_nodes treats a ".Name()" label as file-ish noise on
// purpose, and navigation borrowing that rule excluded every method in the
// graph — the most common definition shape there is. The first fixture never
// caught it because it labels definitions as bare identifiers.
func TestDefinitionsIncludesMethods(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, methodGraph))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := g.Definitions("SearchCodeText", 0)
	if got.Total != 1 || len(got.Definitions) != 1 {
		t.Fatalf("Definitions = %+v (note %q), want the method", got.Definitions, got.Note)
	}
	def := got.Definitions[0]
	if def.Path != "internal/service/manager/docs.go" || def.Line != 1998 {
		t.Errorf("definition = %+v", def)
	}

	// The degree<=1 half of the same rule would have excluded this one instead.
	if lonely := g.Definitions("Lonely", 0); lonely.Total != 1 {
		t.Errorf("a method with a single edge was excluded: %+v (note %q)", lonely.Definitions, lonely.Note)
	}
}

// TestReferencesEmptyContextNamesRelations checks the three empty-reference
// answers stay distinct. Not every edge carries a context — a method's link to
// its receiver has a relation and none — so a node can have real incoming edges
// and an empty context vocabulary at once. Reporting that as "no incoming
// edges" is how a caller concludes a function has no callers.
func TestReferencesEmptyContextNamesRelations(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, methodGraph))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Lonely has one incoming edge, whose relation is "method" and whose
	// context is unset.
	got := g.References("Lonely", []string{"call"}, 0, 0)
	if len(got.References) != 0 {
		t.Fatalf("references = %+v, want none for the filtered context", got.References)
	}
	for _, want := range []string{"carry no context", "method", "Omit contexts"} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("note %q does not mention %q", got.Note, want)
		}
	}
	if strings.Contains(got.Note, "no incoming edges") {
		t.Errorf("note claims the node has no incoming edges: %s", got.Note)
	}

	// And the filter still works when the context does exist.
	calls := g.References("SearchCodeText", []string{"call"}, 0, 0)
	if calls.Total != 1 || calls.References[0].Path != "internal/server/server.go" {
		t.Fatalf("call references = %+v (note %q)", calls.References, calls.Note)
	}
}

// TestResolveSymbolMatchesMethodLabelsExactly is the regression test for the
// tier that method labels never reached. The shared resolver compares a
// tokenised query against the stored norm_label verbatim, so "BlameRepoFile"
// did not match ".blamerepofile()" exactly and fell through to the substring
// tier — where References, which picks one node, chose whichever test function
// contained the name first.
func TestResolveSymbolMatchesMethodLabelsExactly(t *testing.T) {
	t.Parallel()

	const g = `{
  "directed": false, "multigraph": false, "nodes": [
    {"id":"manager.blame","label":".BlameRepoFile()","norm_label":".blamerepofile()","source_file":"internal/service/manager/blame.go","source_location":"L10","file_type":"code"},
    {"id":"test.blame","label":"TestBlameRepoFileGroupsHunks()","norm_label":"testblamerepofilegroupshunks()","source_file":"internal/service/manager/blame_test.go","source_location":"L20","file_type":"code"},
    {"id":"caller","label":".Handler()","norm_label":".handler()","source_file":"internal/server/server.go","source_location":"L5","file_type":"code"}
  ], "links": [
    {"source":"caller","target":"manager.blame","relation":"calls","confidence":"EXTRACTED","context":"call"}
  ]
}`

	graph, err := Load(writeGraph(t, g))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	defs := graph.Definitions("BlameRepoFile", 0)
	if defs.Total != 1 || defs.Definitions[0].ID != "manager.blame" {
		t.Fatalf("definitions = %+v, want only the method", defs.Definitions)
	}
	// The test function stays visible as the weaker match it is.
	if len(defs.Candidates) == 0 {
		t.Error("the substring match was dropped instead of reported as a candidate")
	}

	refs := graph.References("BlameRepoFile", nil, 0, 0)
	if refs.Resolved != "manager.blame" {
		t.Fatalf("references resolved to %q, want the method", refs.Resolved)
	}
	if refs.Total != 1 || refs.References[0].ID != "caller" {
		t.Fatalf("references = %+v", refs.References)
	}
}
