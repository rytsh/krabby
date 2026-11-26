package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

// readSymbolManager tracks one repository whose graph is built by the real
// graph builder, so the test covers bag recording end lines as well as krabby
// reading them.
func readSymbolManager(t *testing.T, files map[string]string) (*Manager, string) {
	t.Helper()

	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, src := range files {
		snapshotTestWrite(t, filepath.Join(clone, filepath.FromSlash(rel)), src)
	}

	if err := graphbuilder.New(time.Minute, nil).Update(context.Background(), clone, nil); err != nil {
		t.Fatalf("build graph: %v", err)
	}

	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	const id = "example.com/team/billing"
	if err := reg.Upsert(context.Background(), &registry.Repo{ID: id, URL: "https://example.com/team/billing.git", Path: clone, Status: registry.StatusReady}); err != nil {
		t.Fatal(err)
	}

	return &Manager{reg: reg, engine: graphquery.NewEngine(0)}, id
}

const invoiceSource = `package billing

// Invoice is a bill.
type Invoice struct {
	Lines []int
}

// Total adds the lines up.
func (i *Invoice) Total() int {
	sum := 0
	for _, l := range i.Lines {
		sum += l
	}

	return sum
}

func Zero() int { return 0 }
`

func TestReadSymbolReturnsTheWholeDeclaration(t *testing.T) {
	m, repo := readSymbolManager(t, map[string]string{"billing/invoice.go": invoiceSource})

	got, err := m.ReadSymbol(context.Background(), repo, "", "Total", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 1 {
		t.Fatalf("symbols = %+v, want one", got.Symbols)
	}

	sym := got.Symbols[0]
	if sym.Path != "billing/invoice.go" || sym.Line != 9 || sym.EndLine != 16 {
		t.Fatalf("location = %s L%d-%d, want billing/invoice.go L9-16", sym.Path, sym.Line, sym.EndLine)
	}
	if sym.Source == nil {
		t.Fatalf("no source: %s", sym.Note)
	}

	want := "func (i *Invoice) Total() int {\n\tsum := 0\n\tfor _, l := range i.Lines {\n\t\tsum += l\n\t}\n\n\treturn sum\n}\n"
	if sym.Source.Content != want {
		t.Errorf("source =\n%s\nwant\n%s", sym.Source.Content, want)
	}
	if sym.Source.Truncated || sym.Note != "" {
		t.Errorf("a short declaration was reported cut: truncated=%v note=%q", sym.Source.Truncated, sym.Note)
	}
	if got.Snapshot == "" {
		t.Error("no snapshot token to continue on the same version")
	}

	typ, err := m.ReadSymbol(context.Background(), repo, "", "Invoice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(typ.Symbols) != 1 || typ.Symbols[0].Source == nil ||
		typ.Symbols[0].Source.Content != "type Invoice struct {\n\tLines []int\n}\n" {
		t.Errorf("type source = %+v", typ.Symbols)
	}
}

// Without an end line the graph knows only where a declaration starts. That
// line is real; anything past it would be a guess.
func TestReadSymbolWithoutAnEndShowsTheFirstLine(t *testing.T) {
	m, repo := readSymbolManager(t, map[string]string{"billing/invoice.go": invoiceSource})

	got, err := m.ReadSymbol(context.Background(), repo, "", "Zero", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 1 || got.Symbols[0].Source == nil {
		t.Fatalf("symbols = %+v", got.Symbols)
	}

	sym := got.Symbols[0]
	if sym.Source.Content != "func Zero() int { return 0 }\n" || sym.EndLine != 0 {
		t.Errorf("one-line declaration = %q end %d", sym.Source.Content, sym.EndLine)
	}
	if !strings.Contains(sym.Note, "does not record where this declaration ends") {
		t.Errorf("note = %q, want it to say the end is unknown", sym.Note)
	}
}

func TestReadSymbolCutsALongDeclaration(t *testing.T) {
	var body strings.Builder
	body.WriteString("package billing\n\nfunc Long() int {\n")
	for range maxSymbolLines + 50 {
		body.WriteString("\t_ = 1\n")
	}
	body.WriteString("\treturn 0\n}\n")

	m, repo := readSymbolManager(t, map[string]string{"billing/long.go": body.String()})

	got, err := m.ReadSymbol(context.Background(), repo, "", "Long", 0)
	if err != nil {
		t.Fatal(err)
	}
	sym := got.Symbols[0]
	if sym.Source == nil || !sym.Source.Truncated {
		t.Fatalf("long declaration not marked truncated: %+v", sym)
	}
	if lines := strings.Count(sym.Source.Content, "\n"); lines != maxSymbolLines {
		t.Errorf("returned %d lines, want %d", lines, maxSymbolLines)
	}
	if !strings.Contains(sym.Note, "Use read_file for the rest") {
		t.Errorf("note = %q", sym.Note)
	}
}

func TestReadSymbolUnknownSymbol(t *testing.T) {
	m, repo := readSymbolManager(t, map[string]string{"billing/invoice.go": invoiceSource})

	got, err := m.ReadSymbol(context.Background(), repo, "", "Refund", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 0 || got.Note == "" {
		t.Errorf("unknown symbol = %+v, want no symbols and a note", got)
	}
}

func TestReadSymbolUnknownRepository(t *testing.T) {
	m, _ := readSymbolManager(t, map[string]string{"billing/invoice.go": invoiceSource})

	if _, err := m.ReadSymbol(context.Background(), "example.com/team/nope", "", "Total", 0); err == nil ||
		!strings.Contains(err.Error(), "unknown repository") {
		t.Fatalf("err = %v, want unknown repository", err)
	}
}

// A common name can have many definitions, each with up to maxSymbolLines of
// source. The answer stays bounded: few definitions by default, and a total
// line budget past which definitions come back as locations only.
func TestReadSymbolBoundsTheAnswer(t *testing.T) {
	long := func(pkg string) string {
		var b strings.Builder
		b.WriteString("package " + pkg + "\n\nfunc Run() int {\n")
		for range maxSymbolLines + 10 {
			b.WriteString("\t_ = 1\n")
		}
		b.WriteString("\treturn 0\n}\n")

		return b.String()
	}

	files := map[string]string{}
	for _, pkg := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		files[pkg+"/run.go"] = long(pkg)
	}
	m, repo := readSymbolManager(t, files)

	got, err := m.ReadSymbol(context.Background(), repo, "", "Run", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 7 || len(got.Symbols) != defaultReadSymbolLimit {
		t.Fatalf("total %d, returned %d; want 7 found and %d returned", got.Total, len(got.Symbols), defaultReadSymbolLimit)
	}

	lines, withSource := 0, 0
	for _, sym := range got.Symbols {
		if sym.Source == nil {
			if !strings.Contains(sym.Note, "line budget") {
				t.Errorf("definition without source has note %q", sym.Note)
			}

			continue
		}
		withSource++
		lines += strings.Count(sym.Source.Content, "\n")
	}
	if lines > maxReadSymbolTotalLines || withSource != maxReadSymbolTotalLines/maxSymbolLines {
		t.Errorf("%d definitions with %d lines of source; budget is %d lines", withSource, lines, maxReadSymbolTotalLines)
	}
}

const receiptSource = `package billing

type Receipt struct{}

func (r *Receipt) Total() int { return 1 }
`

const refundSource = `package billing

func (i *Invoice) Refund() int {
	return -i.Total()
}
`

// A qualified name reads the one Total the caller meant, from a graph the real
// builder made: two types declare Total.
func TestReadSymbolQualifiedByType(t *testing.T) {
	m, repo := readSymbolManager(t, map[string]string{
		"billing/invoice.go": invoiceSource,
		"billing/receipt.go": receiptSource,
		"billing/refund.go":  refundSource,
	})

	bare, err := m.ReadSymbol(context.Background(), repo, "", "Total", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Total != 2 {
		t.Fatalf("bare Total = %d definitions, want 2", bare.Total)
	}

	for symbol, path := range map[string]string{
		"Invoice.Total":              "billing/invoice.go",
		"Receipt::Total":             "billing/receipt.go",
		"billing/receipt.go/Total":   "billing/receipt.go",
		"invoice.go/Invoice.Total()": "billing/invoice.go",
		// Refund is declared on Invoice in a third file.
		"Invoice.Refund":             "billing/refund.go",
		"invoice.go/Invoice.Refund":  "billing/refund.go",
	} {
		got, err := m.ReadSymbol(context.Background(), repo, "", symbol, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 1 || got.Symbols[0].Path != path || got.Symbols[0].Source == nil {
			t.Errorf("%s = %+v (note %q), want the one in %s", symbol, got.Symbols, got.Note, path)
		}
	}
}

func TestFileOutlineFromTheBuiltGraph(t *testing.T) {
	m, repo := readSymbolManager(t, map[string]string{
		"billing/invoice.go": invoiceSource,
		"billing/refund.go":  refundSource,
		"py/shop.py":         "class Cart:\n    def __init__(self):\n        self.items = []\n\n    def total(self):\n        return sum(self.items)\n\ndef helper(x):\n    return x\n",
	})

	got, err := m.FileOutline(context.Background(), repo, "", "billing/invoice.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "billing/invoice.go" || got.Snapshot == "" {
		t.Fatalf("outline = %+v", got)
	}

	found := map[string]graphquery.OutlineSymbol{}
	var walk func([]graphquery.OutlineSymbol)
	walk = func(syms []graphquery.OutlineSymbol) {
		for _, s := range syms {
			found[s.Label] = s
			walk(s.Members)
		}
	}
	walk(got.Symbols)
	for _, want := range []string{"Invoice", ".Total()", "Zero()"} {
		if _, ok := found[want]; !ok {
			t.Errorf("outline lacks %s: %+v", want, got.Symbols)
		}
	}
	if total := found[".Total()"]; total.Line != 9 || total.EndLine != 16 {
		t.Errorf(".Total() = L%d-%d, want L9-16", total.Line, total.EndLine)
	}
	if _, ok := found[".Refund()"]; ok {
		t.Errorf("invoice.go lists Refund, which refund.go declares")
	}

	// A file that only adds a method shows the type it belongs to, and where
	// that type is declared.
	refund, err := m.FileOutline(context.Background(), repo, "", "refund.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(refund.Symbols) != 1 || refund.Symbols[0].Label != "Invoice" || refund.Symbols[0].DeclaredIn != "billing/invoice.go" ||
		len(refund.Symbols[0].Members) != 1 || refund.Symbols[0].Members[0].Label != ".Refund()" {
		t.Errorf("refund.go outline = %+v", refund.Symbols)
	}

	py, err := m.FileOutline(context.Background(), repo, "", "shop.py")
	if err != nil {
		t.Fatal(err)
	}
	if len(py.Symbols) != 2 || py.Symbols[0].Label != "Cart" || len(py.Symbols[0].Members) != 2 || py.Symbols[1].Label != "helper()" {
		t.Errorf("python outline = %+v", py.Symbols)
	}
}
