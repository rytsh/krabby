package graphquery

import (
	"fmt"
	"sort"
	"strings"
)

// maxOutlineSymbols bounds one outline. A generated file can declare
// thousands of symbols, and an outline that long is no cheaper than the file.
const maxOutlineSymbols = 500

// OutlineSymbol is one declaration in a file outline. Members are the
// methods the graph attaches to it that this file declares, in source order.
type OutlineSymbol struct {
	Label   string `json:"label"`
	ID      string `json:"id"`
	Line    int    `json:"line,omitempty"`
	EndLine int    `json:"end_line,omitempty"`
	// DeclaredIn names the file that declares a type whose methods this file
	// adds to (Go spreads one type's methods over many files). Line and
	// EndLine are then that declaration's, in that file.
	DeclaredIn string          `json:"declared_in,omitempty"`
	Members    []OutlineSymbol `json:"members,omitempty"`
}

// FileOutline is the declarations of one source file, without their bodies.
type FileOutline struct {
	Path    string          `json:"path,omitempty"`
	Symbols []OutlineSymbol `json:"symbols"`
	// Total counts the file's declarations, members included, also those
	// left out past maxOutlineSymbols.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated,omitempty"`
	// Candidates lists the files a path matched when it named more than one.
	Candidates []string `json:"candidates,omitempty"`
	// Snapshot continues on the same version with read_file.
	Snapshot string `json:"snapshot,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Outline lists the declarations of the file path names: top-level functions,
// types and classes in source order, each with the methods the graph links to
// it. path is repo-relative, or a unique tail of it ("manager/codenav.go").
//
// It reads only the graph, so it costs no file read and shows what read_symbol
// and find_references can answer for that file. A file the graph has no
// declarations for (a language bag does not parse, or one with none) answers
// with a note rather than an empty list that looks like an empty file.
func (g *Graph) Outline(path string) FileOutline {
	out := FileOutline{Symbols: []OutlineSymbol{}}

	files := g.filesMatching(path)
	switch {
	case len(files) == 0:
		out.Note = fmt.Sprintf("No file in the graph matches '%s'. Use glob to find its path; a file in a language the graph does not parse has no outline, so read_file it instead.",
			sanitize(path))

		return out
	case len(files) > 1:
		for _, f := range files[:min(len(files), maxSymbolCandidates)] {
			out.Candidates = append(out.Candidates, sanitize(f))
		}
		out.Note = fmt.Sprintf("'%s' matches %d files; retry with one of the candidates.", sanitize(path), len(files))
		if len(files) > maxSymbolCandidates {
			out.Note += fmt.Sprintf(" Only the first %d are listed: give more of the path.", maxSymbolCandidates)
		}

		return out
	}

	file := files[0]
	out.Path = file

	o := g.collectOutline(file)
	out.Total = len(o.declared)

	budget := maxOutlineSymbols
	out.Symbols = o.render(g, o.top, &budget)

	// Declarations only reachable through a cycle of method edges have no
	// root among the top level; they are listed there rather than dropped.
	var orphans []string
	for _, id := range g.files[file] {
		if o.declared[id] && !o.visited[id] {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) > 0 && budget > 0 {
		out.Symbols = append(out.Symbols, o.render(g, orphans, &budget)...)
	}
	out.Truncated = o.rendered < out.Total

	switch {
	case out.Total == 0:
		out.Note = fmt.Sprintf("The graph records no declarations in '%s'. It may declare none, or be in a language the graph does not parse: read_file it instead.",
			sanitize(file))
	case out.Truncated:
		out.Note = fmt.Sprintf("The file declares %d symbols; showing the first %d. Use read_file for the rest.",
			out.Total, o.rendered)
	}

	return out
}

// outline is one file's declarations arranged as a tree before rendering.
type outline struct {
	file string
	// declared is every definition whose source_file is the file.
	declared map[string]bool
	// members lists, per owner, the methods of the file attached to it.
	members map[string][]string
	top     []string
	// rendered counts the declarations of the file that made it into the
	// answer; an owner declared elsewhere is context, not one of them.
	rendered int
	// visited stops a node being rendered twice, which a cycle of method
	// edges (A -method-> B -method-> A) would otherwise do forever.
	visited map[string]bool
}

func (g *Graph) collectOutline(file string) *outline {
	o := &outline{file: file, declared: map[string]bool{}, members: map[string][]string{}, visited: map[string]bool{}}

	for _, id := range g.files[file] {
		if g.definitionKind(id) == kindSymbol {
			o.declared[id] = true
		}
	}

	isMember := map[string]bool{}
	var foreignOwners []string
	for _, id := range g.files[file] {
		if !o.declared[id] {
			continue
		}
		for _, r := range g.in[id] {
			owner := r.other
			if r.edge.Relation != relationMethod || owner == id || isMember[id] || g.definitionKind(owner) != kindSymbol {
				continue
			}
			isMember[id] = true
			if !o.declared[owner] && len(o.members[owner]) == 0 {
				foreignOwners = append(foreignOwners, owner)
			}
			o.members[owner] = append(o.members[owner], id)
		}
	}

	for _, id := range g.files[file] {
		if o.declared[id] && !isMember[id] {
			o.top = append(o.top, id)
		}
	}
	o.top = append(o.top, foreignOwners...)

	return o
}

// render emits ids and, recursively, their members in source order. Each of
// the file's own declarations spends one unit of budget; an owner from
// another file is shown for its members and costs nothing.
func (o *outline) render(g *Graph, ids []string, budget *int) []OutlineSymbol {
	type located struct {
		id      string
		line    int
		end     int
		foreign bool
		// sortLine orders an owner from another file by its first member,
		// where its methods appear in this file.
		sortLine int
	}

	sorted := make([]located, 0, len(ids))
	for _, id := range ids {
		line, end := g.location(id)
		l := located{id: id, line: line, end: end, sortLine: line, foreign: !o.declared[id]}
		if l.foreign {
			l.sortLine = 0
			for _, m := range o.members[id] {
				if ml, _ := g.location(m); ml > 0 && (l.sortLine == 0 || ml < l.sortLine) {
					l.sortLine = ml
				}
			}
		}
		sorted = append(sorted, l)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].sortLine, sorted[j].sortLine
		// A declaration with no known line goes last rather than first.
		if (a == 0) != (b == 0) {
			return b == 0
		}

		return a < b
	})

	out := make([]OutlineSymbol, 0, len(sorted))
	for _, s := range sorted {
		if *budget <= 0 {
			break
		}
		if o.visited[s.id] {
			continue
		}
		o.visited[s.id] = true
		if !s.foreign {
			*budget--
			o.rendered++
		}

		sym := OutlineSymbol{
			Label:   sanitize(strings.TrimSpace(g.labelOf(s.id))),
			ID:      s.id,
			Line:    s.line,
			EndLine: s.end,
		}
		if s.foreign {
			sym.DeclaredIn = sanitize(g.Nodes[s.id].SourceFile)
		}
		if m := o.members[s.id]; len(m) > 0 {
			sym.Members = o.render(g, m, budget)
		}
		if s.foreign && len(sym.Members) == 0 {
			continue
		}
		out = append(out, sym)
	}

	return out
}
