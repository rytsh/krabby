package graphquery

import (
	"path"
	"sort"
	"strings"
)

// ImportCount is one external dependency and how many non-test files import it.
type ImportCount struct {
	Label string
	Files int
}

// goTLDs mark the domain segment of a third-party Go import path once dots
// have been flattened to underscores ("github_com_...", "google_golang_org_...").
var goTLDs = map[string]bool{"com": true, "org": true, "io": true, "in": true, "dev": true, "net": true, "cloud": true, "co": true, "ai": true}

// ExternalImports lists dependencies imported from outside the repository,
// ranked by the number of distinct non-test source files importing them. Go
// standard-library packages are omitted except network and database ones,
// which describe how a service communicates.
func (g *Graph) ExternalImports(limit int) []ImportCount {
	files := map[string]map[string]struct{}{}
	for _, refs := range g.out {
		for _, ref := range refs {
			if ref.edge.Relation != "imports" && ref.edge.Relation != "imports_from" {
				continue
			}
			source, target := g.Nodes[ref.edge.Source], g.Nodes[ref.edge.Target]
			if source == nil || target == nil || target.SourceFile != "" || source.SourceFile == "" || isTestPath(source.SourceFile) {
				continue
			}
			label, ok := externalLabel(target.Label)
			if !ok {
				continue
			}
			if files[label] == nil {
				files[label] = map[string]struct{}{}
			}
			files[label][source.SourceFile] = struct{}{}
		}
	}
	out := make([]ImportCount, 0, len(files))
	for label, set := range files {
		out = append(out, ImportCount{Label: label, Files: len(set)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Label < out[j].Label
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func externalLabel(label string) (string, bool) {
	if label == "" || strings.HasPrefix(label, "ref_node_") || strings.Contains(label, "()") {
		return "", false
	}
	rest, isGo := strings.CutPrefix(label, "go_pkg_")
	if !isGo {
		return label, true
	}
	segments := strings.Split(rest, "_")
	if strings.HasPrefix(rest, "net") || strings.HasPrefix(rest, "database") {
		return "go:" + rest, true
	}
	for i := 1; i < len(segments) && i <= 2; i++ {
		if goTLDs[segments[i]] {
			return "go:" + rest, true
		}
	}
	return "", false
}

// TopEntities returns labels of the most-connected code entities (types,
// functions), skipping file, concept, JSON-key and test-file nodes.
func (g *Graph) TopEntities(limit int) []string {
	var out []string
	for _, id := range g.nodesByDegreeDesc() {
		if len(out) >= limit {
			break
		}
		node := g.Nodes[id]
		if g.isFileNode(id) || g.isConceptNode(id) || g.isJSONKeyNode(id) || isTestPath(node.SourceFile) || looksLikeFile(node.Label) {
			continue
		}
		out = append(out, g.labelOf(id))
	}
	return out
}

// looksLikeFile catches file nodes labelled with a relative path, which
// isFileNode (base-name equality) misses.
func looksLikeFile(label string) bool {
	return strings.Contains(label, "/") || path.Ext(label) != "" && !strings.HasSuffix(label, ")")
}

func isTestPath(file string) bool {
	lower := strings.ToLower(file)
	base := path.Base(lower)
	return strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.Contains(lower, "/test/") || strings.Contains(lower, "/tests/") || strings.HasPrefix(lower, "test/") || strings.HasPrefix(lower, "tests/")
}
