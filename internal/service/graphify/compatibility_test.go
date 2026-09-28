package graphify_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
)

func TestClientReportsVersion(t *testing.T) {
	client := graphify.New(time.Minute, nil)
	if got := client.Version(); got != graphify.TestedVersion {
		t.Fatalf("Version() = %q, want %q", got, graphify.TestedVersion)
	}
	if client.GraphBuiltWithCurrentVersion(t.TempDir()) {
		t.Fatal("missing graph version marker reported as current")
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "graphify-out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := client.RecordGraphVersion(repo); err != nil {
		t.Fatal(err)
	}
	if !client.GraphBuiltWithCurrentVersion(repo) {
		t.Fatal("recorded graph version did not report as current")
	}
}

func TestBagLibraryCompatibility(t *testing.T) {
	client := graphify.New(2*time.Minute, nil)
	if got := client.Version(); got != graphify.TestedVersion {
		t.Fatalf("bag engine version = %q, tested version is %q", got, graphify.TestedVersion)
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
		if err := client.Update(ctx, repo, nil); err != nil {
			t.Fatalf("bag build %s: %v", name, err)
		}
		graphPath := graphify.GraphPath(repo)
		if err := graphquery.Validate(graphPath); err != nil {
			t.Fatalf("validate %s graph: %v", name, err)
		}
		graphs = append(graphs, graphPath)
	}

	merged := filepath.Join(t.TempDir(), "merged.json")
	if err := client.MergeGraphs(ctx, merged, graphs...); err != nil {
		t.Fatalf("bag merge graphs: %v", err)
	}
	if err := graphquery.Validate(merged); err != nil {
		t.Fatalf("validate merged graph: %v", err)
	}
}
