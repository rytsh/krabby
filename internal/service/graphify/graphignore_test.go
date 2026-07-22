package graphify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readIgnore(t *testing.T, clone string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(clone, ignoreFileName))
	if err != nil {
		t.Fatalf("read .graphifyignore: %v", err)
	}

	return string(b)
}

func TestWriteIgnoreCreatesManagedBlock(t *testing.T) {
	clone := t.TempDir()

	changed, err := WriteIgnore(clone, []string{"custom/", "*.foo"})
	if err != nil {
		t.Fatalf("WriteIgnore: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on first write")
	}

	content := readIgnore(t, clone)

	if !strings.Contains(content, managedBegin) || !strings.Contains(content, managedEnd) {
		t.Fatalf("managed markers missing:\n%s", content)
	}
	if !strings.Contains(content, "testdata/") {
		t.Errorf("default pattern testdata/ missing:\n%s", content)
	}
	if !strings.Contains(content, "custom/") || !strings.Contains(content, "*.foo") {
		t.Errorf("extra patterns missing:\n%s", content)
	}
}

func TestWriteIgnoreIdempotent(t *testing.T) {
	clone := t.TempDir()

	if _, err := WriteIgnore(clone, []string{"x/"}); err != nil {
		t.Fatalf("first WriteIgnore: %v", err)
	}

	changed, err := WriteIgnore(clone, []string{"x/"})
	if err != nil {
		t.Fatalf("second WriteIgnore: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when content is unchanged")
	}
}

func TestWriteIgnorePreservesUserContent(t *testing.T) {
	clone := t.TempDir()

	// A user maintains their own patterns above the managed block.
	userContent := "# my rules\nsecret.txt\nlocal/\n"
	if err := os.WriteFile(filepath.Join(clone, ignoreFileName), []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteIgnore(clone, nil); err != nil {
		t.Fatalf("WriteIgnore: %v", err)
	}

	content := readIgnore(t, clone)

	for _, want := range []string{"# my rules", "secret.txt", "local/"} {
		if !strings.Contains(content, want) {
			t.Errorf("user line %q not preserved:\n%s", want, content)
		}
	}
	if !strings.Contains(content, "testdata/") {
		t.Errorf("managed defaults not appended:\n%s", content)
	}

	// Rewriting must not duplicate the user content or the managed block.
	if _, err := WriteIgnore(clone, nil); err != nil {
		t.Fatalf("second WriteIgnore: %v", err)
	}
	content = readIgnore(t, clone)

	if n := strings.Count(content, "secret.txt"); n != 1 {
		t.Errorf("user content duplicated %d times:\n%s", n, content)
	}
	if n := strings.Count(content, managedBegin); n != 1 {
		t.Errorf("managed block duplicated %d times:\n%s", n, content)
	}
}

func TestHasManagedIgnore(t *testing.T) {
	clone := t.TempDir()

	if HasManagedIgnore(clone) {
		t.Fatal("expected false before any ignore file exists")
	}

	// A user-only ignore file (no managed block) must not count.
	if err := os.WriteFile(filepath.Join(clone, ignoreFileName), []byte("local/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasManagedIgnore(clone) {
		t.Fatal("expected false for a user-only ignore file")
	}

	if _, err := WriteIgnore(clone, nil); err != nil {
		t.Fatalf("WriteIgnore: %v", err)
	}
	if !HasManagedIgnore(clone) {
		t.Fatal("expected true after writing the managed block")
	}
}

func TestMatchesExcluded(t *testing.T) {
	patterns := mergedPatterns(nil)
	cases := map[string]bool{
		"pkg/plugins/extractor/xml2/testdata/result/sub/output_sub_5.json": true,
		"testdata/mock/db.go":     true,
		"a/b/fixtures/c.json":     true,
		"foo/__mocks__/bar.js":    true,
		"lib.min.js":              true,
		"internal/service/foo.go": false,
		"cmd/main.go":             false,
		"pkg/testdatabase/x.go":   false, // substring must NOT match a dir segment
		"exporter/testdatatofile.go": false,
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

func TestWriteIgnoreUpdatesWhenExtraChanges(t *testing.T) {
	clone := t.TempDir()

	if _, err := WriteIgnore(clone, []string{"one/"}); err != nil {
		t.Fatalf("WriteIgnore one: %v", err)
	}

	changed, err := WriteIgnore(clone, []string{"two/"})
	if err != nil {
		t.Fatalf("WriteIgnore two: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when extra patterns change")
	}

	content := readIgnore(t, clone)
	if strings.Contains(content, "one/") {
		t.Errorf("stale pattern one/ still present:\n%s", content)
	}
	if !strings.Contains(content, "two/") {
		t.Errorf("new pattern two/ missing:\n%s", content)
	}
}
