package manager

import (
	"context"
	"fmt"

	"github.com/rytsh/krabby/internal/service/graphquery"
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
func (m *Manager) resolveGraphRepo(ctx context.Context, repoID, namespace, tool string, args map[string]any) (resolved, selection string, err error) {
	if repoID != "" || m.mergeEnabled {
		return repoID, "", nil
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
