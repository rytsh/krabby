package graphbuilder_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/graphquery"
)

func TestClientReportsVersion(t *testing.T) {
	builder := graphbuilder.New(time.Minute, nil)
	if got := builder.Version(); got != graphbuilder.TestedVersion {
		t.Fatalf("Version() = %q, want %q", got, graphbuilder.TestedVersion)
	}
	if builder.GraphBuiltWithCurrentVersion(t.TempDir()) {
		t.Fatal("missing graph version marker reported as current")
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "graphify-out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := builder.RecordGraphVersion(repo); err != nil {
		t.Fatal(err)
	}
	if !builder.GraphBuiltWithCurrentVersion(repo) {
		t.Fatal("recorded graph version did not report as current")
	}
}

func TestExcludeDoesNotExposeBuilderConfiguration(t *testing.T) {
	input := []string{"vendor/**"}
	builder := graphbuilder.New(time.Minute, input)
	input[0] = "input-mutated"
	got := builder.Exclude()
	if got[0] != "vendor/**" {
		t.Fatalf("constructor retained input slice: %v", got)
	}
	got[0] = "result-mutated"
	if next := builder.Exclude(); next[0] != "vendor/**" {
		t.Fatalf("Exclude exposed internal slice: %v", next)
	}
	if got := graphbuilder.New(time.Minute, nil).Exclude(); got != nil {
		t.Fatalf("nil excludes = %v, want nil", got)
	}
}

func TestBagLibraryCompatibility(t *testing.T) {
	builder := graphbuilder.New(2*time.Minute, nil)
	if got := builder.Version(); got != graphbuilder.TestedVersion {
		t.Fatalf("bag engine version = %q, tested version is %q", got, graphbuilder.TestedVersion)
	}

	ctx := context.Background()
	graphs := make([]string, 0, 2)
	for _, name := range []string{"alpha", "beta"} {
		repo := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		source := "def " + name + "():\n    return \"" + name + "\"\n"
		if err := os.WriteFile(filepath.Join(repo, name+".py"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, "testdata"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "testdata", "ignored.py"), []byte("def should_not_exist(): pass\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := builder.Update(ctx, repo, nil); err != nil {
			t.Fatalf("bag build %s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(repo, ".graphifyignore")); !os.IsNotExist(err) {
			t.Fatalf("build wrote .graphifyignore: %v", err)
		}
		graphPath := graphbuilder.GraphPath(repo)
		if err := graphquery.Validate(graphPath); err != nil {
			t.Fatalf("validate %s graph: %v", name, err)
		}
		graphJSON, err := os.ReadFile(graphPath)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(graphJSON, []byte("should_not_exist")) {
			t.Fatal("default excludes were not passed to bag")
		}
		graphs = append(graphs, graphPath)
	}

	merged := filepath.Join(t.TempDir(), "merged.json")
	if err := builder.MergeGraphs(ctx, merged, graphs...); err != nil {
		t.Fatalf("bag merge graphs: %v", err)
	}
	if err := graphquery.Validate(merged); err != nil {
		t.Fatalf("validate merged graph: %v", err)
	}
}
