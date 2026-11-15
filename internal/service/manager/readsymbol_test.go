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
