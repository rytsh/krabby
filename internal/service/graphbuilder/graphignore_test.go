package graphbuilder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveLegacyManagedIgnore(t *testing.T) {
	clone := t.TempDir()
	path := filepath.Join(clone, legacyIgnoreFileName)
	content := "# my rules\nlocal/\n\n" + managedBegin + "\ntestdata/\n" + managedEnd + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := RemoveLegacyManagedIgnore(clone)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected legacy block to be removed")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "# my rules\nlocal/\n"; got != want {
		t.Fatalf("preserved content = %q, want %q", got, want)
	}
}

func TestRemoveLegacyManagedIgnoreDeletesGeneratedFile(t *testing.T) {
	clone := t.TempDir()
	path := filepath.Join(clone, legacyIgnoreFileName)
	content := managedBegin + "\ntestdata/\n" + managedEnd + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := RemoveLegacyManagedIgnore(clone)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected generated file to be removed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy file still exists: %v", err)
	}
}

func TestRemoveLegacyManagedIgnoreLeavesUserFileAlone(t *testing.T) {
	clone := t.TempDir()
	path := filepath.Join(clone, legacyIgnoreFileName)
	if err := os.WriteFile(path, []byte("local/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := RemoveLegacyManagedIgnore(clone)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("user-authored file was changed")
	}
}

func TestMatchesExcluded(t *testing.T) {
	patterns := mergedPatterns(nil)
	cases := map[string]bool{
		"pkg/plugins/extractor/xml2/testdata/result/sub/output_sub_5.json": true,
		"testdata/mock/db.go":                    true,
		"a/b/fixtures/c.json":                    true,
		"foo/__mocks__/bar.js":                   true,
		"lib.min.js":                             true,
		"vendor/github.com/pkg/errors/errors.go": true,
		"vendor/modules.txt":                     true,
		"internal/service/foo.go":                false,
		"cmd/main.go":                            false,
		"pkg/testdatabase/x.go":                  false, // substring must NOT match a dir segment
		"exporter/testdatatofile.go":             false,
	}
	for rel, want := range cases {
		if got := matchesExcluded(rel, patterns); got != want {
			t.Errorf("matchesExcluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestGraphHasExcludedNodes(t *testing.T) {
	clone := t.TempDir()
	outDir := filepath.Join(clone, "graphify-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A clean graph: no excluded nodes.
	clean := `{"nodes":[{"source_file":"internal/svc/a.go"},{"source_file":"cmd/main.go"}],"links":[]}`
	if err := os.WriteFile(filepath.Join(outDir, "graph.json"), []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}
	if GraphHasExcludedNodes(clone, nil) {
		t.Error("clean graph reported excluded nodes")
	}

	// A dirty graph with a nested testdata node.
	dirty := `{"nodes":[{"source_file":"pkg/x/testdata/result/out.json"}],"links":[]}`
	if err := os.WriteFile(filepath.Join(outDir, "graph.json"), []byte(dirty), 0o644); err != nil {
		t.Fatal(err)
	}
	if !GraphHasExcludedNodes(clone, nil) {
		t.Error("dirty graph with testdata node not detected")
	}
}

// A repository's own patterns are unioned with the install-wide ones rather
// than replacing them: an install-wide rule is a policy, and a repo opting out
// of it is not a case worth supporting.
func TestPerRepoIgnorePatternsUnion(t *testing.T) {
	c := &Builder{exclude: []string{"global/"}}

	got := c.ignorePatterns([]string{"repo/"})
	if len(got) != 2 || got[0] != "global/" || got[1] != "repo/" {
		t.Fatalf("ignorePatterns = %v, want both lists", got)
	}

	// No repo patterns: the install-wide list is returned as-is.
	if got := c.ignorePatterns(nil); len(got) != 1 || got[0] != "global/" {
		t.Fatalf("ignorePatterns(nil) = %v", got)
	}

	// The install-wide slice is shared by every repo built in this process, so
	// unioning must not append into it.
	base := &Builder{exclude: make([]string, 1, 8)}
	base.exclude[0] = "global/"
	_ = base.ignorePatterns([]string{"repo/"})
	if len(base.exclude) != 1 || base.exclude[0] != "global/" {
		t.Fatalf("install-wide excludes mutated: %v", base.exclude)
	}
}
