package openapi

import (
	"strings"

	"go.yaml.in/yaml/v4"
)

// maxNodeStrings bounds how many enum values are carried into the catalog. A
// long enum (currency codes, country codes) is noise in a request recipe: the
// first few show the shape, and the rest belong in the spec.
const maxNodeStrings = 20

// nodeString renders a YAML scalar node as a plain string. Non-scalars (an
// object-valued example, say) render as "" rather than as a mangled fragment,
// because a half-rendered structure in a parameter table is worse than no
// example at all.
func nodeString(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	if node.Kind != yaml.ScalarNode {
		return ""
	}

	return strings.TrimSpace(node.Value)
}

// nodeStrings renders a list of scalar nodes, dropping empties and stopping at
// maxNodeStrings.
func nodeStrings(nodes []*yaml.Node) []string {
	if len(nodes) == 0 {
		return nil
	}

	out := make([]string, 0, min(len(nodes), maxNodeStrings))
	for _, node := range nodes {
		if len(out) >= maxNodeStrings {
			break
		}
		if v := nodeString(node); v != "" {
			out = append(out, v)
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}
