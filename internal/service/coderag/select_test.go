package coderag

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/config"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSelectFilesIndexesDeployConfig covers the gap that made a
// deployment-only repository invisible to every search tool: compose and CI
// files carry the image versions per environment, but no extension in
// defaultIncludeExts matches them and their names vary, so nothing was
// selected and both the semantic and the BM25 code index came out empty.
func TestSelectFilesIndexesDeployConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, "docker-compose.yml", "services:\n")
	writeFile(t, root, "docker-compose.prod.yml", "services:\n  api:\n    image: acme/api:2.14.3\n")
	writeFile(t, root, "deploy/docker-compose.sandbox.yaml", "services:\n")
	writeFile(t, root, "charts/api/values-stage.yaml", "image:\n  tag: 2.15.0\n")
	writeFile(t, root, ".gitlab-ci.yml", "stages:\n")
	writeFile(t, root, ".github/workflows/release.yml", "on: push\n")
	// Arbitrary YAML stays out: matching families rather than the extension
	// is what keeps generated manifests out of the index.
	writeFile(t, root, "k8s/deployment.yaml", "kind: Deployment\n")
	writeFile(t, root, "openapi.yaml", "openapi: 3.0.0\n")

	svc := New(config.CodeRAG{}, nil, nil, nil, nil)

	files, err := svc.selectFiles(root, config.Filters{})
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}

	for _, want := range []string{
		"main.go",
		"docker-compose.yml",
		"docker-compose.prod.yml",
		"deploy/docker-compose.sandbox.yaml",
		"charts/api/values-stage.yaml",
		".gitlab-ci.yml",
		".github/workflows/release.yml",
	} {
		if !got[want] {
			t.Errorf("expected %q to be indexed, got %v", want, files)
		}
	}

	for _, skip := range []string{"k8s/deployment.yaml", "openapi.yaml"} {
		if got[skip] {
			t.Errorf("expected %q to stay out of the default allowlist, got %v", skip, files)
		}
	}
}

// An explicit Include replaces the built-in allowlist entirely, deploy config
// included: the user is in control.
func TestSelectFilesExplicitIncludeReplacesDefaults(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, "docker-compose.yml", "services:\n")

	svc := New(config.CodeRAG{Filters: config.Filters{Include: []string{"*.go"}}}, nil, nil, nil, nil)

	files, err := svc.selectFiles(root, svc.cfg.Filters)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "main.go" {
		t.Fatalf("explicit Include should select only main.go, got %v", files)
	}
}

// TestSelectFilesRepoOverrides covers the per-repository escape hatch: a repo
// whose content the install-wide allowlist does not describe.
func TestSelectFilesRepoOverrides(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, "k8s/deployment.yaml", "kind: Deployment\n")
	writeFile(t, root, "generated/api.yaml", "openapi: 3.0.0\n")

	svc := New(config.CodeRAG{}, nil, nil, nil, nil)

	// include_extra widens the built-in allowlist rather than replacing it, so
	// the Go source is still indexed.
	files, err := svc.selectFiles(root, config.Filters{IncludeExtra: []string{"**/*.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got["main.go"] || !got["k8s/deployment.yaml"] {
		t.Fatalf("include_extra should add to the defaults, got %v", files)
	}

	// Exclude is applied last and wins over include_extra.
	files, err = svc.selectFiles(root, config.Filters{
		IncludeExtra: []string{"**/*.yaml"},
		Exclude:      []string{"generated/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "generated/api.yaml" {
			t.Fatalf("exclude must win over include_extra, got %v", files)
		}
	}
}
