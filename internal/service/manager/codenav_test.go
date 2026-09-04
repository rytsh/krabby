package manager

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

// codeNavGraph defines Handle in two files and records one call into the first.
const codeNavGraph = `{
  "directed": false, "multigraph": false, "nodes": [
    {"id":"pkga.Handle","label":"Handle","norm_label":"handle","source_file":"pkga/a.go","source_location":"L10-L20","file_type":"code"},
    {"id":"pkgb.Handle","label":"Handle","norm_label":"handle","source_file":"pkgb/b.go","source_location":"L30","file_type":"code"},
    {"id":"pkgx.Caller","label":"Caller","norm_label":"caller","source_file":"pkgx/caller.go","source_location":"L7","file_type":"code"}
  ], "links": [
    {"source":"pkgx.Caller","target":"pkga.Handle","relation":"calls","confidence":"EXTRACTED","context":"call"}
  ]
}`

// codeNavManager returns a manager whose registry holds one ready graph per id.
func codeNavManager(t *testing.T, repoIDs ...string) *Manager {
	t.Helper()

	dataDir := t.TempDir()
	db, err := storage.Open(filepath.Join(dataDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}

	for i, id := range repoIDs {
		path := filepath.Join(dataDir, "repos", "r"+string(rune('a'+i)))
		mustWriteManagerTest(t, graphify.GraphPath(path), codeNavGraph)
		if err := reg.Upsert(context.Background(), &registry.Repo{
			ID: id, URL: "https://" + id, Branch: "main", Path: path, Status: registry.StatusReady,
		}); err != nil {
			t.Fatal(err)
		}
	}

	return &Manager{reg: reg, engine: graphquery.NewEngine(0)}
}

// With one ready graph in the namespace the repository is implied, and both
// lookups answer from that graph.
func TestFindDefinitionAndReferencesResolveTheOnlyRepo(t *testing.T) {
	m := codeNavManager(t, "example.com/team/one")

	defs, err := m.FindDefinition(context.Background(), "", "", "Handle", 0)
	if err != nil {
		t.Fatalf("FindDefinition: %v", err)
	}
	if defs.Total != 2 || len(defs.Definitions) != 2 {
		t.Fatalf("definitions = %+v (total %d), want both defining files", defs.Definitions, defs.Total)
	}

	refs, err := m.FindReferences(context.Background(), "", "", "Handle", []string{"call"}, 1, 50)
	if err != nil {
		t.Fatalf("FindReferences: %v", err)
	}
	if refs.Total != 1 || refs.References[0].ID != "pkgx.Caller" || refs.References[0].Line != 7 {
		t.Fatalf("references = %+v, want the caller at line 7", refs.References)
	}
}

// An ambiguous repository must surface CallGraphTool's selection instruction as
// the next action instead of guessing a repository or failing opaquely.
func TestFindDefinitionReportsRepoSelectionInNote(t *testing.T) {
	m := codeNavManager(t, "example.com/team/one", "example.com/team/two")

	defs, err := m.FindDefinition(context.Background(), "", "", "Handle", 0)
	if err != nil {
		t.Fatalf("FindDefinition: %v", err)
	}
	if len(defs.Definitions) != 0 {
		t.Fatalf("definitions = %+v, want none until a repo is chosen", defs.Definitions)
	}
	for _, want := range []string{"Repository selection required", "Retry find_definition with repo"} {
		if !strings.Contains(defs.Note, want) {
			t.Errorf("note missing %q: %s", want, defs.Note)
		}
	}

	refs, err := m.FindReferences(context.Background(), "", "", "Handle", nil, 1, 50)
	if err != nil {
		t.Fatalf("FindReferences: %v", err)
	}
	if !strings.Contains(refs.Note, "Retry find_references with repo") {
		t.Errorf("references note missing the selection instruction: %s", refs.Note)
	}

	// Naming the repository resolves the ambiguity.
	chosen, err := m.FindDefinition(context.Background(), "example.com/team/two", "", "Handle", 0)
	if err != nil {
		t.Fatalf("FindDefinition with repo: %v", err)
	}
	if chosen.Total != 2 || chosen.Note != "" {
		t.Fatalf("explicit repo lookup = %+v note %q", chosen.Definitions, chosen.Note)
	}
}

// No graph at all is an error, not an empty result: the caller must know the
// repository has not been indexed yet.
func TestFindDefinitionWithoutAnyGraphErrors(t *testing.T) {
	m := codeNavManager(t)

	if _, err := m.FindDefinition(context.Background(), "", "", "Handle", 0); err == nil {
		t.Fatal("FindDefinition succeeded with no repository graph")
	} else if !strings.Contains(err.Error(), "no repository graph is ready") {
		t.Fatalf("unexpected error: %v", err)
	}
}
