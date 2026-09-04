package graphquery

import (
	"strings"
	"testing"
)

func TestNormalizeTraversalMode(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ModeBFS},
		{in: ModeBFS, want: ModeBFS},
		{in: ModeDFS, want: ModeDFS},
		{in: "depth-first", wantErr: true},
		{in: "DFS", wantErr: true},
		{in: "breadth", wantErr: true},
	} {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeTraversalMode(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeTraversalMode(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err != nil {
				for _, want := range []string{ModeBFS, ModeDFS} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name the accepted mode %q", err, want)
					}
				}

				return
			}
			if got != tt.want {
				t.Fatalf("NormalizeTraversalMode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The traversal itself must not silently degrade either: reaching QueryGraph
// with an unvalidated mode used to produce a breadth-first subgraph under a
// header naming the mode the caller asked for, which is indistinguishable from
// a real answer. Empty still selects the default traversal.
func TestQueryGraphRefusesUnknownMode(t *testing.T) {
	g := loadSmall(t)

	got := g.QueryGraph("Service", QueryGraphOpts{Mode: "depth-first"})
	if strings.Contains(got, "Traversal:") {
		t.Fatalf("an unknown mode produced a traversal answer:\n%s", got)
	}
	for _, want := range []string{"depth-first", ModeBFS, ModeDFS} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal %q does not mention %q", got, want)
		}
	}

	if def := g.QueryGraph("Service", QueryGraphOpts{}); !strings.HasPrefix(def, "Traversal: BFS depth=3") {
		t.Fatalf("empty mode must still run the default traversal:\n%s", def)
	}
	if dfs := g.QueryGraph("Service", QueryGraphOpts{Mode: ModeDFS}); !strings.HasPrefix(dfs, "Traversal: DFS depth=3") {
		t.Fatalf("dfs must still run:\n%s", dfs)
	}
}
