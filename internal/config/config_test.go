package config

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNormalizeBasePath(t *testing.T) {
	for in, want := range map[string]string{
		"":             "",
		"/":            "",
		"  ":           "",
		"krabby":       "/krabby",
		"/krabby":      "/krabby",
		"/krabby/":     "/krabby",
		" /krabby/ ":   "/krabby",
		"//a///b//":    "/a/b",
		"tools/krabby": "/tools/krabby",
	} {
		if got := NormalizeBasePath(in); got != want {
			t.Errorf("NormalizeBasePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Records written before a setting existed, or carrying a value this version
// does not know, must keep behaving rather than fail to load.
func TestModeParsersFallBackToTheDefault(t *testing.T) {
	for in, want := range map[string]Capture{
		"off": CaptureOff, " Truncated ": CaptureTruncated, "full": CaptureFull,
		"": CaptureFull, "everything": CaptureFull,
	} {
		if got := ParseCapture(in); got != want {
			t.Errorf("ParseCapture(%q) = %q, want %q", in, got, want)
		}
	}

	for in, want := range map[string]EmbedInputMode{
		"single": EmbedInputSingle, " SINGLE ": EmbedInputSingle,
		"batch": EmbedInputBatch, "": EmbedInputBatch, "streaming": EmbedInputBatch,
	} {
		if got := ParseEmbedInputMode(in); got != want {
			t.Errorf("ParseEmbedInputMode(%q) = %q, want %q", in, got, want)
		}
	}

	for in, want := range map[string]EmbedTaskMode{
		"retrieval": EmbedTaskRetrieval, " Retrieval ": EmbedTaskRetrieval,
		"standard": EmbedTaskStandard, "": EmbedTaskStandard, "other": EmbedTaskStandard,
	} {
		if got := ParseEmbedTaskMode(in); got != want {
			t.Errorf("ParseEmbedTaskMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for in, want := range map[string]string{
		"~":              home,
		"~/.krabby":      filepath.Join(home, ".krabby"),
		"/data":          "/data",
		"relative/dir":   "relative/dir",
		"~other/.krabby": "~other/.krabby", // another user's home is not ours to guess
		"/srv/~/data":    "/srv/~/data",
	} {
		got, err := expandHome(in)
		if err != nil || got != want {
			t.Errorf("expandHome(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// Every store lives under the data directory and none of them share a path:
// two stores opened on one directory would corrupt each other.
func TestDataDirectoriesAreDistinctAndContained(t *testing.T) {
	cfg := &Config{DataDir: "/data"}

	dirs := map[string]string{
		"repos":        cfg.ReposDir(),
		"state":        cfg.StateDir(),
		"keys":         cfg.KeysDir(),
		"docs":         cfg.DocsRootDir(),
		"docs vectors": cfg.DocsVectorsDir(),
		"code vectors": cfg.CodeVectorsDir(),
		"sources":      cfg.SourcesRootDir(),
		"apis":         cfg.APIsRootDir(),
		"big pictures": cfg.BigPicturesRootDir(),
		"merged graph": filepath.Dir(cfg.MergedGraphPath()),
	}

	seen := map[string]string{}
	for name, dir := range dirs {
		if rel, err := filepath.Rel("/data", dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			t.Errorf("%s directory %q is not inside the data directory", name, dir)
		}
		if other, dup := seen[dir]; dup {
			t.Errorf("%s and %s share the directory %q", name, other, dir)
		}
		seen[dir] = name
	}

	if got := cfg.MergedGraphPath(); filepath.Base(got) != "graph.json" {
		t.Errorf("MergedGraphPath = %q, want a graph.json", got)
	}
}

// loadFrom runs Load against a config file and environment the test controls.
func loadFrom(t *testing.T, yaml string, env map[string]string) *Config {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Keep ambient settings of the machine running the tests out of the
	// result. t.Setenv registers the restore; Unsetenv then removes the
	// variable for this test, since an empty value is not the same as unset.
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "KRABBY_") {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}

	path := filepath.Join(dir, "krabby.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", path)
	t.Chdir(dir)

	for k, v := range env {
		t.Setenv(k, v)
	}

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

func TestLoadDefaults(t *testing.T) {
	cfg := loadFrom(t, "{}\n", nil)

	home, _ := os.UserHomeDir()
	if cfg.DataDir != filepath.Join(home, ".krabby") {
		t.Errorf("data_dir = %q, want ~/.krabby expanded", cfg.DataDir)
	}
	if cfg.Server.Port != "8080" || cfg.Server.BasePath != "" {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.MCP.Path != "/mcp" {
		t.Errorf("mcp.path = %q", cfg.MCP.Path)
	}
	if cfg.Bag.BuildTimeout != 30*time.Minute {
		t.Errorf("bag.build_timeout = %v", cfg.Bag.BuildTimeout)
	}

	// The two defaults that are security decisions: closed until an operator
	// opens them.
	if cfg.Server.Pprof {
		t.Error("pprof is on by default")
	}
	if len(cfg.Server.AllowOrigins) != 0 {
		t.Errorf("allow_origins defaults to %q, want no origin allowed", cfg.Server.AllowOrigins)
	}
	if cfg.Bag.Merge {
		t.Error("cross-repo merge is on by default")
	}
}

func TestLoadFromFile(t *testing.T) {
	cfg := loadFrom(t, `
data_dir: /srv/krabby
server:
  port: "9090"
  base_path: krabby/
  allow_origins:
    - " https://tools.example.com/ "
    - https://tools.example.com
    - "https://*.corp.example"
bag:
  build_timeout: 5m
  exclude: ["examples/", "*.gen.ts"]
`, nil)

	if cfg.DataDir != "/srv/krabby" {
		t.Errorf("data_dir = %q", cfg.DataDir)
	}
	if cfg.Server.Port != "9090" {
		t.Errorf("port = %q", cfg.Server.Port)
	}
	if cfg.Server.BasePath != "/krabby" {
		t.Errorf("base_path = %q, want it normalized", cfg.Server.BasePath)
	}
	if want := []string{"https://tools.example.com", "https://*.corp.example"}; !slices.Equal(cfg.Server.AllowOrigins, want) {
		t.Errorf("allow_origins = %q, want %q", cfg.Server.AllowOrigins, want)
	}
	if cfg.Bag.BuildTimeout != 5*time.Minute || !slices.Equal(cfg.Bag.Exclude, []string{"examples/", "*.gen.ts"}) {
		t.Errorf("bag = %+v", cfg.Bag)
	}
}

// The container is configured through the environment, so it has to win over
// the file and be able to express every setting the README documents.
func TestEnvironmentOverridesFile(t *testing.T) {
	cfg := loadFrom(t, "server:\n  port: \"9090\"\n  base_path: /from-file\n", map[string]string{
		"KRABBY_SERVER_PORT":          "7070",
		"KRABBY_SERVER_BASE_PATH":     "/from-env/",
		"KRABBY_SERVER_ALLOW_ORIGINS": "https://a.example.com, https://b.example.com/,",
		"KRABBY_DATA_DIR":             "~/data",
	})

	if cfg.Server.Port != "7070" {
		t.Errorf("port = %q, want the environment's", cfg.Server.Port)
	}
	if cfg.Server.BasePath != "/from-env" {
		t.Errorf("base_path = %q, want the environment's, normalized", cfg.Server.BasePath)
	}
	if want := []string{"https://a.example.com", "https://b.example.com"}; !slices.Equal(cfg.Server.AllowOrigins, want) {
		t.Errorf("allow_origins = %q, want %q", cfg.Server.AllowOrigins, want)
	}

	home, _ := os.UserHomeDir()
	if cfg.DataDir != filepath.Join(home, "data") {
		t.Errorf("data_dir = %q, want ~ expanded", cfg.DataDir)
	}
}
