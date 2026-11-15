package manager

import (
	"context"
	"fmt"

	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/repofs"
)

// FindDefinition resolves where a symbol is defined in the selected repository's
// knowledge graph. When the repository is ambiguous the selection instruction is
// returned in Note rather than as an error, so the caller sees the next action
// instead of a failed lookup.
func (m *Manager) FindDefinition(ctx context.Context, repoID, namespace, symbol string, limit int) (graphquery.SymbolDefs, error) {
	graphPath, selection, err := m.graphPathForQuery(ctx, repoID, namespace, "find_definition", map[string]any{"symbol": symbol})
	if err != nil {
		return graphquery.SymbolDefs{}, err
	}
	if selection != "" {
		return graphquery.SymbolDefs{Symbol: symbol, Definitions: []graphquery.Definition{}, Note: selection}, nil
	}

	defs, err := m.engine.Definitions(graphPath, symbol, limit)
	if err != nil {
		return graphquery.SymbolDefs{}, fmt.Errorf("finding definitions of %s; %w", symbol, err)
	}

	return defs, nil
}

// FindReferences resolves where a symbol is used in the selected repository's
// knowledge graph, paginated. Repository ambiguity is reported the same way as in
// FindDefinition.
func (m *Manager) FindReferences(ctx context.Context, repoID, namespace, symbol string, contexts []string, page, perPage int) (graphquery.SymbolRefs, error) {
	graphPath, selection, err := m.graphPathForQuery(ctx, repoID, namespace, "find_references", map[string]any{"symbol": symbol})
	if err != nil {
		return graphquery.SymbolRefs{}, err
	}
	if selection != "" {
		return graphquery.SymbolRefs{Symbol: symbol, References: []graphquery.Reference{}, Note: selection}, nil
	}

	refs, err := m.engine.References(graphPath, symbol, contexts, page, perPage)
	if err != nil {
		return graphquery.SymbolRefs{}, fmt.Errorf("finding references to %s; %w", symbol, err)
	}

	return refs, nil
}

// graphPathForQuery resolves the graph file a symbol query runs against, applying
// the same repository selection rules as CallGraphTool. A non-empty selection
// means the caller must surface that instruction and not query.
func (m *Manager) graphPathForQuery(ctx context.Context, repoID, namespace, tool string, args map[string]any) (graphPath, selection string, err error) {
	repoID, selection, err = m.resolveGraphRepo(ctx, repoID, namespace, tool, args)
	if err != nil || selection != "" {
		return "", selection, err
	}

	graphPath, err = m.GraphPathFor(ctx, repoID)
	if err != nil {
		return "", "", err
	}

	return graphPath, "", nil
}

// resolveGraphRepo picks the repository whose graph a query runs against. An
// explicit repoID wins; with merge disabled and no repoID the namespace's ready
// graphs decide: one candidate is used, an id named in the arguments is inferred,
// and a genuinely ambiguous set yields the "Repository selection required"
// instruction instead of a silent guess at the wrong repository.
//
// A named repository may be a unique suffix of its id ("acme/api" for
// "github.com/acme/api"), as everywhere else a repo is named.
func (m *Manager) resolveGraphRepo(ctx context.Context, repoID, namespace, tool string, args map[string]any) (resolved, selection string, err error) {
	if repoID != "" {
		return m.resolveRepoRef(ctx, repoID)
	}
	if m.mergeEnabled {
		return "", "", nil
	}

	repoIDs, err := m.graphRepoIDs(ctx, namespace)
	if err != nil {
		return "", "", err
	}

	switch len(repoIDs) {
	case 0:
		return "", "", fmt.Errorf("no repository graph is ready in namespace %s; add a repository, wait for its build to finish, or retry with namespace \"*\"", displayNamespace(namespace))
	case 1:
		return repoIDs[0], "", nil
	default:
		if inferred := inferRepoID(repoIDs, args); inferred != "" {
			return inferred, "", nil
		}

		return "", graphRepoSelectionText(tool, namespace, repoIDs), nil
	}
}

// maxSymbolLines bounds one read_symbol answer. A declaration longer than this
// is cut and marked truncated; the caller pages the rest with read_file.
const maxSymbolLines = 400

// read_symbol returns source, not just locations, so it answers far fewer
// definitions than find_definition: a common name ("String", "New") can have
// dozens, and each could carry maxSymbolLines of code.
const (
	defaultReadSymbolLimit = 5
	maxReadSymbolLimit     = 20
	// maxReadSymbolTotalLines bounds the source across all definitions in one
	// answer. Definitions past it keep their location and get a note.
	maxReadSymbolTotalLines = 1200
)

// SymbolSource is one definition of a symbol together with its source.
type SymbolSource struct {
	graphquery.Definition
	// Source is the declaration's text, from Line through EndLine (or as far
	// as maxSymbolLines allows).
	Source *repofs.LineRange `json:"source,omitempty"`
	// Note explains why Source is missing or shorter than the declaration.
	Note string `json:"note,omitempty"`
}

// SymbolSources is what ReadSymbol returns.
type SymbolSources struct {
	Symbol  string         `json:"symbol"`
	Symbols []SymbolSource `json:"symbols"`
	// Total counts every definition found, including those past limit.
	Total      int      `json:"total"`
	Candidates []string `json:"candidates,omitempty"`
	Snapshot   string   `json:"snapshot,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// ReadSymbol returns the source of every definition of symbol: the graph says
// where each declaration starts and ends, and the lines are read from the
// repository's active clone. A definition whose end the graph does not know
// (an older graph, a language bag cannot bound) is returned from its start
// line with a note, rather than guessed.
//
// It needs one repository's graph, because a definition's path is only
// meaningful inside the clone it came from. A cross-repository merged graph
// therefore does not answer it, even when merging is enabled.
func (m *Manager) ReadSymbol(ctx context.Context, repoID, namespace, symbol string, limit int) (SymbolSources, error) {
	out := SymbolSources{Symbol: symbol, Symbols: []SymbolSource{}}

	repo, selection, err := m.singleGraphRepo(ctx, repoID, namespace, symbol)
	if err != nil {
		return out, err
	}
	if selection != "" {
		out.Note = selection

		return out, nil
	}

	// The graph and the source come from the same snapshot directory, read
	// once: resolving them separately would let a refresh that finishes in
	// between pair one version's line numbers with another version's file.
	// The token lets a caller continue with read_file on that version.
	dir, token, err := m.repoCloneDirAt(ctx, repo, "")
	if err != nil {
		return out, err
	}

	graphPath := graphbuilder.GraphPath(dir)
	if !fileExists(graphPath) {
		return out, notFound("graph for %s not built yet", repo)
	}

	switch {
	case limit <= 0:
		limit = defaultReadSymbolLimit
	case limit > maxReadSymbolLimit:
		limit = maxReadSymbolLimit
	}

	defs, err := m.engine.Definitions(graphPath, symbol, limit)
	if err != nil {
		return out, fmt.Errorf("finding definitions of %s; %w", symbol, err)
	}

	out.Total, out.Candidates, out.Note = defs.Total, defs.Candidates, defs.Note
	if len(defs.Definitions) == 0 {
		return out, nil
	}
	out.Snapshot = token

	budget := maxReadSymbolTotalLines
	for _, def := range defs.Definitions {
		if budget <= 0 {
			out.Symbols = append(out.Symbols, SymbolSource{
				Definition: def,
				Note:       "source left out: this answer reached its line budget. Call read_symbol with repo and a narrower symbol, or read_file at this location.",
			})

			continue
		}

		sym := readDefinition(dir, def, min(budget, maxSymbolLines))
		if sym.Source != nil {
			budget -= sym.Source.End - sym.Source.Start + 1
		}
		out.Symbols = append(out.Symbols, sym)
	}

	return out, nil
}

// singleGraphRepo resolves the one repository read_symbol runs against. It
// applies the usual selection rules, except that a merged graph never stands
// in for a repository: with merging enabled and no repo given, the choice is
// made the same way it would be without merging.
// resolveRepoRef resolves a repository named by id or unique id suffix.
func (m *Manager) resolveRepoRef(ctx context.Context, ref string) (id, selection string, err error) {
	r, err := m.reg.Resolve(ctx, ref)
	if err != nil {
		return "", "", err
	}
	if r == nil {
		return "", "", notFound("unknown repository %q; use list_repos for its exact id", ref)
	}

	return r.ID, "", nil
}

func (m *Manager) singleGraphRepo(ctx context.Context, repoID, namespace, symbol string) (repo, selection string, err error) {
	if repoID != "" {
		return m.resolveRepoRef(ctx, repoID)
	}

	repoIDs, err := m.graphRepoIDs(ctx, namespace)
	if err != nil {
		return "", "", err
	}

	switch len(repoIDs) {
	case 0:
		return "", "", fmt.Errorf("no repository graph is ready in namespace %s; add a repository, wait for its build to finish, or retry with namespace \"*\"", displayNamespace(namespace))
	case 1:
		return repoIDs[0], "", nil
	default:
		if inferred := inferRepoID(repoIDs, map[string]any{"symbol": symbol}); inferred != "" {
			return inferred, "", nil
		}

		return "", graphRepoSelectionText("read_symbol", namespace, repoIDs), nil
	}
}

// readDefinition reads one definition's source, at most maxLines of it.
func readDefinition(dir string, def graphquery.Definition, maxLines int) SymbolSource {
	out := SymbolSource{Definition: def}

	switch {
	case def.Path == "" || def.Line == 0:
		out.Note = "the graph records no location for this definition"

		return out
	case def.EndLine == 0:
		// Without an end the declaration is one line, or one whose extent the
		// graph builder could not tell; either way its first line is real.
		out.Note = "the graph does not record where this declaration ends; showing its first line. Use read_file for the rest."
		def.EndLine = def.Line
	}

	end := min(def.EndLine, def.Line+maxLines-1)

	src, err := repofs.ReadLines(dir, def.Path, def.Line, end)
	if err != nil {
		out.Note = "source unavailable: " + err.Error()

		return out
	}
	out.Source = src

	if end < def.EndLine {
		src.Truncated = true
		out.Note = fmt.Sprintf("declaration is %d lines; showing the first %d. Use read_file for the rest.",
			def.EndLine-def.Line+1, end-def.Line+1)
	}

	return out
}
