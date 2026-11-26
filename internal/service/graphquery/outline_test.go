package graphquery

import (
	"fmt"
	"strings"
	"testing"
)

// outlineGraph is the shape bag writes for Go: a file node that contains the
// file's top-level declarations, one node per type at its declaration, and a
// method edge from that type to each of its methods wherever they are
// declared - here Cart.String lives in cart_string.go. A file whose name
// collides with another is labelled with its path ("order/cart.go"). Two
// types share the method names String and Total, and both live in a
// "cart.go", so neither a bare name nor a bare file name is unique.
const outlineGraph = `{
  "directed": false, "multigraph": false, "nodes": [
    {"id":"shop_cart_go","label":"shop/cart.go","norm_label":"shop/cart.go","source_file":"shop/cart.go","source_location":"L1","file_type":"code"},
    {"id":"shop_cart","label":"Cart","norm_label":"cart","source_file":"shop/cart.go","source_location":"L5","end_line":8,"file_type":"code"},
    {"id":"shop_cart_total","label":".Total()","norm_label":".total()","source_file":"shop/cart.go","source_location":"L20","end_line":25,"file_type":"code"},
    {"id":"shop_cart_add","label":".Add()","norm_label":".add()","source_file":"shop/cart.go","source_location":"L10","end_line":18,"file_type":"code"},
    {"id":"shop_newcart","label":"NewCart()","norm_label":"newcart()","source_file":"shop/cart.go","source_location":"L27","end_line":29,"file_type":"code"},
    {"id":"shop_cart_string_go","label":"cart_string.go","norm_label":"cart_string.go","source_file":"shop/cart_string.go","source_location":"L1","file_type":"code"},
    {"id":"shop_cart_string","label":".String()","norm_label":".string()","source_file":"shop/cart_string.go","source_location":"L3","end_line":5,"file_type":"code"},
    {"id":"order_cart_go","label":"order/cart.go","norm_label":"order/cart.go","source_file":"order/cart.go","source_location":"L1","file_type":"code"},
    {"id":"order_order","label":"Order","norm_label":"order","source_file":"order/cart.go","source_location":"L3","end_line":6,"file_type":"code"},
    {"id":"order_order_string","label":".String()","norm_label":".string()","source_file":"order/cart.go","source_location":"L8","end_line":10,"file_type":"code"},
    {"id":"order_order_total","label":".Total()","norm_label":".total()","source_file":"order/cart.go","source_location":"L12","end_line":14,"file_type":"code"},
    {"id":"cart_stub","label":"Cart","norm_label":"cart","source_file":"","source_location":"","file_type":"code"},
    {"id":"helper","label":"helper()","norm_label":"helper()","source_file":"shop/cart_test.go","source_location":"L3","file_type":"code"},
    {"id":"readme","label":"README.md","norm_label":"readme.md","source_file":"README.md","source_location":"L1","file_type":"document"}
  ], "links": [
    {"source":"shop_cart_go","target":"shop_cart","relation":"contains","confidence":"EXTRACTED"},
    {"source":"shop_cart_go","target":"shop_newcart","relation":"contains","confidence":"EXTRACTED"},
    {"source":"shop_cart","target":"shop_cart_total","relation":"method","confidence":"EXTRACTED"},
    {"source":"shop_cart","target":"shop_cart_add","relation":"method","confidence":"EXTRACTED"},
    {"source":"shop_cart","target":"shop_cart_string","relation":"method","confidence":"EXTRACTED"},
    {"source":"order_cart_go","target":"order_order","relation":"contains","confidence":"EXTRACTED"},
    {"source":"order_order","target":"order_order_string","relation":"method","confidence":"EXTRACTED"},
    {"source":"order_order","target":"order_order_total","relation":"method","confidence":"EXTRACTED"},
    {"source":"shop_newcart","target":"shop_cart_total","relation":"calls","confidence":"EXTRACTED","context":"call"},
    {"source":"shop_newcart","target":"shop_cart","relation":"references","confidence":"EXTRACTED","context":"return_type"},
    {"source":"helper","target":"cart_stub","relation":"references","confidence":"EXTRACTED","context":"return_type"}
  ]
}`

func loadOutline(t *testing.T) *Graph {
	t.Helper()
	g, err := Load(writeGraph(t, outlineGraph))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return g
}

func labels(syms []OutlineSymbol) string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Label)
	}

	return strings.Join(out, ",")
}

// An outline lists the file's declarations in source order, methods nested
// under their type, with no bodies and no file node.
func TestOutlineListsDeclarationsInSourceOrderWithMembers(t *testing.T) {
	t.Parallel()

	got := loadOutline(t).Outline("shop/cart.go")
	if got.Path != "shop/cart.go" || got.Note != "" {
		t.Fatalf("outline = %+v", got)
	}
	if got.Total != 4 {
		t.Errorf("total = %d, want 4 declarations", got.Total)
	}
	if l := labels(got.Symbols); l != "Cart,NewCart()" {
		t.Fatalf("top level = %s, want the type then the constructor", l)
	}

	cart := got.Symbols[0]
	if cart.Line != 5 || cart.EndLine != 8 || cart.DeclaredIn != "" {
		t.Errorf("Cart = %+v, want L5-8 declared here", cart)
	}
	// String belongs to Cart but is declared in another file.
	if l := labels(cart.Members); l != ".Add(),.Total()" {
		t.Fatalf("Cart members = %s, want this file's methods Add then Total", l)
	}
	if m := cart.Members[1]; m.Line != 20 || m.EndLine != 25 {
		t.Errorf("Total = L%d-%d, want L20-25", m.Line, m.EndLine)
	}
}

// A file that only adds methods to a type declared elsewhere shows that type
// as their owner, marked with where it is declared, and does not count it.
func TestOutlineShowsTheOwnerOfMethodsDeclaredElsewhere(t *testing.T) {
	t.Parallel()

	got := loadOutline(t).Outline("cart_string.go")
	if got.Total != 1 || len(got.Symbols) != 1 {
		t.Fatalf("outline = %+v, want one owner holding one method", got)
	}

	owner := got.Symbols[0]
	if owner.Label != "Cart" || owner.DeclaredIn != "shop/cart.go" || owner.Line != 5 {
		t.Errorf("owner = %+v, want Cart declared in shop/cart.go at L5", owner)
	}
	if l := labels(owner.Members); l != ".String()" {
		t.Errorf("members = %s", l)
	}
	if got.Truncated {
		t.Error("an outline of one method reported truncated")
	}
}

// A file named by a tail matching several files must not be guessed.
func TestOutlineAmbiguousPathListsCandidates(t *testing.T) {
	t.Parallel()

	got := loadOutline(t).Outline("cart.go")
	if len(got.Symbols) != 0 || strings.Join(got.Candidates, ",") != "order/cart.go,shop/cart.go" {
		t.Fatalf("outline = %+v, want no symbols and both cart.go files, sorted", got)
	}
	if !strings.Contains(got.Note, "2 files") {
		t.Errorf("note = %q", got.Note)
	}

	if tail := loadOutline(t).Outline("./order/cart.go"); tail.Path != "order/cart.go" || labels(tail.Symbols) != "Order" {
		t.Errorf("a unique tail did not resolve: %+v", tail)
	}
}

// An empty outline must say why: an unknown path and a file without parsed
// declarations are different answers, and neither means "the file is empty".
func TestOutlineExplainsAnEmptyAnswer(t *testing.T) {
	t.Parallel()

	g := loadOutline(t)

	if missing := g.Outline("shop/nope.go"); missing.Path != "" || !strings.Contains(missing.Note, "glob") {
		t.Errorf("unknown path = %+v", missing)
	}

	doc := g.Outline("README.md")
	if doc.Path != "README.md" || len(doc.Symbols) != 0 || !strings.Contains(doc.Note, "read_file") {
		t.Errorf("file without declarations = %+v", doc)
	}
}

func TestOutlineIsBounded(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(`{"directed":false,"multigraph":false,"nodes":[`)
	b.WriteString(`{"id":"f","label":"big.go","source_file":"big.go","source_location":"L1"}`)
	b.WriteString(`,{"id":"t","label":"T","source_file":"big.go","source_location":"L2"}`)
	for i := range maxOutlineSymbols + 20 {
		fmt.Fprintf(&b, `,{"id":"s%d","label":".F%d()","source_file":"big.go","source_location":"L%d"}`, i, i, i+3)
	}
	b.WriteString(`],"links":[`)
	for i := range maxOutlineSymbols + 20 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"source":"t","target":"s%d","relation":"method"}`, i)
	}
	b.WriteString(`]}`)

	g, err := Load(writeGraph(t, b.String()))
	if err != nil {
		t.Fatal(err)
	}

	got := g.Outline("big.go")
	shown := 0
	for _, s := range got.Symbols {
		shown += 1 + len(s.Members)
	}
	if shown != maxOutlineSymbols || !got.Truncated || got.Total != maxOutlineSymbols+21 {
		t.Fatalf("outline of %d symbols showed %d (truncated %v, total %d)", maxOutlineSymbols+21, shown, got.Truncated, got.Total)
	}
	if !strings.Contains(got.Note, fmt.Sprintf("showing the first %d", maxOutlineSymbols)) {
		t.Errorf("note = %q", got.Note)
	}
}

// A qualified name picks the one definition a bare name cannot: String and
// Total are each defined on two types.
func TestDefinitionsResolveNamesQualifiedByType(t *testing.T) {
	t.Parallel()

	g := loadOutline(t)

	if bare := g.Definitions("String", 0); bare.Total != 2 {
		t.Fatalf("bare String = %d definitions, want 2", bare.Total)
	}

	for _, tc := range []struct {
		symbol string
		want   string
	}{
		{"Cart.String", "shop_cart_string"},
		{"Cart/String", "shop_cart_string"},
		{"Cart::String", "shop_cart_string"},
		{"Cart#String", "shop_cart_string"},
		{"Order.String()", "order_order_string"},
		{"order.total", "order_order_total"},
		{"Cart.Total", "shop_cart_total"},
	} {
		got := g.Definitions(tc.symbol, 0)
		if got.Total != 1 || got.Definitions[0].ID != tc.want {
			t.Errorf("%s = %+v (note %q), want only %s", tc.symbol, got.Definitions, got.Note, tc.want)
		}
	}
}

func TestDefinitionsResolveNamesQualifiedByFile(t *testing.T) {
	t.Parallel()

	g := loadOutline(t)

	for symbol, want := range map[string]string{
		"order/cart.go/Total":       "order_order_total",
		"cart_string.go/String":     "shop_cart_string",
		"shop/cart.go/Cart.Total":   "shop_cart_total",
		"order/cart.go/Order.Total": "order_order_total",
		// The file declares the method, the type lives elsewhere.
		"cart_string.go/Cart.String": "shop_cart_string",
		// The file declares the type, the method lives elsewhere.
		"shop/cart.go/Cart.String": "shop_cart_string",
	} {
		got := g.Definitions(symbol, 0)
		if got.Total != 1 || got.Definitions[0].ID != want {
			t.Errorf("%s = %+v (note %q), want only %s", symbol, got.Definitions, got.Note, want)
		}
	}

	// A file qualifier that names two files answers from both.
	if both := g.Definitions("cart.go/Total", 0); both.Total != 2 {
		t.Errorf("cart.go/Total = %d definitions, want both", both.Total)
	}
	// A file that neither declares the type nor the method narrows to nothing.
	if none := g.Definitions("order/cart.go/Cart.String", 0); none.Total != 0 {
		t.Errorf("order/cart.go/Cart.String = %+v, want nothing", none.Definitions)
	}

	refs := g.References("Cart.Total", nil, 0, 0)
	if refs.Resolved != "shop_cart_total" || refs.Total != 2 {
		t.Errorf("references to Cart.Total = %+v, want its type's method edge and the call", refs)
	}
}

// Inherited methods answer through the type that inherits them.
func TestQualifiedNameFindsInheritedMethods(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, `{"directed":false,"multigraph":false,"nodes":[
    {"id":"base","label":"Base","source_file":"m.py","source_location":"L1"},
    {"id":"base_save","label":".save()","source_file":"m.py","source_location":"L2"},
    {"id":"child","label":"Child","source_file":"m.py","source_location":"L5"},
    {"id":"child_run","label":".run()","source_file":"m.py","source_location":"L6"},
    {"id":"loop","label":"Loop","source_file":"m.py","source_location":"L9"}
  ],"links":[
    {"source":"base","target":"base_save","relation":"method"},
    {"source":"child","target":"child_run","relation":"method"},
    {"source":"child","target":"base","relation":"inherits"},
    {"source":"loop","target":"loop","relation":"inherits"}
  ]}`))
	if err != nil {
		t.Fatal(err)
	}

	if got := g.Definitions("Child.save", 0); got.Total != 1 || got.Definitions[0].ID != "base_save" {
		t.Errorf("Child.save = %+v (note %q)", got.Definitions, got.Note)
	}
	if got := g.Definitions("Loop.save", 0); got.Total != 0 {
		t.Errorf("a self-inheriting type resolved %+v", got.Definitions)
	}
}

// An override hides the method it overrides.
func TestQualifiedNamePrefersTheOverride(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, `{"directed":false,"multigraph":false,"nodes":[
    {"id":"base","label":"Base","source_file":"m.py","source_location":"L1"},
    {"id":"base_save","label":".save()","source_file":"m.py","source_location":"L2"},
    {"id":"child","label":"Child","source_file":"m.py","source_location":"L5"},
    {"id":"child_save","label":".save()","source_file":"m.py","source_location":"L6"}
  ],"links":[
    {"source":"base","target":"base_save","relation":"method"},
    {"source":"child","target":"child_save","relation":"method"},
    {"source":"child","target":"base","relation":"inherits"}
  ]}`))
	if err != nil {
		t.Fatal(err)
	}

	if got := g.Definitions("Child.save", 0); got.Total != 1 || got.Definitions[0].ID != "child_save" {
		t.Errorf("Child.save = %+v, want only the override", got.Definitions)
	}
}

// A label that really is dotted is the exact answer, ahead of a qualified
// reading of the same text.
func TestExactDottedLabelBeatsTheQualifiedReading(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, `{"directed":false,"multigraph":false,"nodes":[
    {"id":"handler","label":"http.Handler","source_file":"x.go","source_location":"L1"},
    {"id":"http","label":"http","source_file":"y.go","source_location":"L1"},
    {"id":"http_handler","label":".Handler()","source_file":"y.go","source_location":"L2"}
  ],"links":[
    {"source":"http","target":"http_handler","relation":"method"}
  ]}`))
	if err != nil {
		t.Fatal(err)
	}

	if got := g.Definitions("http.Handler", 0); got.Total != 1 || got.Definitions[0].ID != "handler" {
		t.Errorf("http.Handler = %+v, want the node labelled so", got.Definitions)
	}
}

// Go names a package function by its package directory.
func TestQualifiedNameByGoPackage(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, `{"directed":false,"multigraph":false,"nodes":[
    {"id":"mgr_new","label":"New()","source_file":"internal/service/manager/manager.go","source_location":"L10"},
    {"id":"reg_new","label":"New()","source_file":"internal/service/registry/registry.go","source_location":"L10"},
    {"id":"t","label":"T","source_file":"internal/service/manager/t.go","source_location":"L1"},
    {"id":"t_new","label":".New()","source_file":"internal/service/manager/t.go","source_location":"L3"}
  ],"links":[
    {"source":"t","target":"t_new","relation":"method"}
  ]}`))
	if err != nil {
		t.Fatal(err)
	}

	for symbol, want := range map[string]string{
		"manager.New":          "mgr_new",
		"service/registry.New": "reg_new",
		`service\manager.New`:  "mgr_new",
	} {
		got := g.Definitions(symbol, 0)
		if got.Total != 1 || got.Definitions[0].ID != want {
			t.Errorf("%s = %+v (note %q), want only %s", symbol, got.Definitions, got.Note, want)
		}
	}
}

func TestOutlineAcceptsWindowsPaths(t *testing.T) {
	t.Parallel()

	if got := loadOutline(t).Outline(`shop\cart.go`); got.Path != "shop/cart.go" {
		t.Errorf("outline = %+v", got)
	}
}

// A cycle of method edges must neither loop nor lose declarations.
func TestOutlineSurvivesAMethodCycle(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, `{"directed":false,"multigraph":false,"nodes":[
    {"id":"a","label":"A","source_file":"m.py","source_location":"L1"},
    {"id":"b","label":"B","source_file":"m.py","source_location":"L5"},
    {"id":"c","label":"c()","source_file":"m.py","source_location":"L9"}
  ],"links":[
    {"source":"a","target":"b","relation":"method"},
    {"source":"b","target":"a","relation":"method"}
  ]}`))
	if err != nil {
		t.Fatal(err)
	}

	got := g.Outline("m.py")
	shown := 0
	var count func([]OutlineSymbol)
	count = func(syms []OutlineSymbol) {
		for _, s := range syms {
			shown++
			count(s.Members)
		}
	}
	count(got.Symbols)
	if got.Total != 3 || shown != 3 || got.Truncated {
		t.Errorf("outline = %+v: shown %d of %d, truncated %v", got.Symbols, shown, got.Total, got.Truncated)
	}
}

// A name that only looks qualified, or whose owner is unknown, falls back to
// plain resolution instead of answering nothing.
func TestQualifiedNameFallsBackWhenTheOwnerIsUnknown(t *testing.T) {
	t.Parallel()

	g := loadOutline(t)

	// "Basket" is no type, so the plain resolver answers: no exact match for
	// "basket.string", and a note rather than a silent empty list.
	if got := g.Definitions("Basket.String", 0); got.Total != 0 || got.Note == "" {
		t.Errorf("unknown owner = %+v", got)
	}
	if got := g.Definitions(".String()", 0); got.Total != 2 {
		t.Errorf("method label form = %d definitions, want both String methods", got.Total)
	}

	for _, s := range []string{"Cart.", ".", "/String", ".String()", "Cart.a b"} {
		if _, _, ok := splitQualified(s); ok {
			t.Errorf("splitQualified(%q) accepted a non-qualified name", s)
		}
	}
}

// References belong to one node. Between a definition and a sourceless stub
// of the same name - what a builder leaves for a name it could not resolve -
// the definition is the one asked about, and the stub stays a candidate.
func TestReferencesPreferTheDefinitionOverAStub(t *testing.T) {
	t.Parallel()

	g := loadOutline(t)

	refs := g.References("Cart", []string{"return_type"}, 0, 0)
	if refs.Resolved != "shop_cart" {
		t.Fatalf("resolved = %q, want the declaration", refs.Resolved)
	}
	if refs.Total != 1 || refs.References[0].ID != "shop_newcart" {
		t.Errorf("references = %+v", refs.References)
	}

	found := false
	for _, c := range refs.Candidates {
		if strings.Contains(c, "cart_stub") {
			found = true
		}
	}
	if !found {
		t.Errorf("the stub is not reported as a candidate: %v", refs.Candidates)
	}
}

// Graphs built before bag folded Go receivers have one node per file that
// declares methods on a type, all with the type's label. Until such a graph
// is rebuilt, a qualified name collects the methods of every one of them, and
// an outline shows them under the receiver of their own file.
func TestQualifiedNameOnAGraphWithReceiverNodes(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, `{"directed":false,"multigraph":false,"nodes":[
    {"id":"cart_go","label":"cart.go","source_file":"shop/cart.go","source_location":"L1"},
    {"id":"cart","label":"Cart","source_file":"shop/cart.go","source_location":"L3","end_line":5},
    {"id":"total_go","label":"total.go","source_file":"shop/total.go","source_location":"L1"},
    {"id":"total_go_cart","label":"Cart","source_file":"shop/total.go","source_location":"L3"},
    {"id":"cart_total","label":".Total()","source_file":"shop/total.go","source_location":"L3","end_line":6}
  ],"links":[
    {"source":"cart_go","target":"cart","relation":"contains"},
    {"source":"total_go_cart","target":"cart_total","relation":"method"}
  ]}`))
	if err != nil {
		t.Fatal(err)
	}

	if got := g.Definitions("Cart.Total", 0); got.Total != 1 || got.Definitions[0].ID != "cart_total" {
		t.Errorf("Cart.Total = %+v (note %q)", got.Definitions, got.Note)
	}

	o := g.Outline("shop/total.go")
	if labels(o.Symbols) != "Cart" || labels(o.Symbols[0].Members) != ".Total()" || o.Total != 2 {
		t.Errorf("outline = %+v", o)
	}
}

// bag labels a file whose name is ambiguous with its path; that node is the
// file, not a definition, and is not listed in its own outline.
func TestPathLabelledFileNodeIsNotADefinition(t *testing.T) {
	t.Parallel()

	g := loadOutline(t)
	if kind := g.definitionKind("order_cart_go"); kind != kindFile {
		t.Errorf("order/cart.go node kind = %s, want file", kind)
	}
	if defs := g.Definitions("order/cart.go", 0); len(defs.Definitions) != 0 {
		t.Errorf("the file node was returned as a definition: %+v", defs.Definitions)
	}
}

// A repeated node id moves the node between files in the index, so an
// outline never lists a node under a file it no longer claims.
func TestRepeatedNodeIDMovesBetweenFiles(t *testing.T) {
	t.Parallel()

	g, err := Load(writeGraph(t, `{"directed":false,"multigraph":false,"nodes":[
    {"id":"x","label":"X","source_file":"a.go","source_location":"L1"},
    {"id":"x","label":"X","source_file":"b.go","source_location":"L2"}
  ],"links":[]}`))
	if err != nil {
		t.Fatal(err)
	}

	if a := g.Outline("a.go"); a.Path != "" || len(a.Symbols) != 0 {
		t.Errorf("a.go still lists the moved node: %+v", a)
	}
	if b := g.Outline("b.go"); labels(b.Symbols) != "X" {
		t.Errorf("b.go = %+v", b)
	}
}
