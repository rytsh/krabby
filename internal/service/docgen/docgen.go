// Package docgen turns a tracked repository into human-readable markdown
// documentation. The default generator prompts an LLM per file/package using the
// graphify graph plus source content, and writes markdown under krabby-docs/
// alongside a docs-index.json manifest.
//
// SCAFFOLD: interfaces, types and the manifest shape are final; llmGenerator's
// generation logic is a stub.
package docgen

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/llm"
)

// DocMeta describes one generated markdown document.
type DocMeta struct {
	Path       string    `json:"path"`        // repo-relative path under krabby-docs/, e.g. "exporter/exporter.go.md"
	Title      string    `json:"title"`       // human title
	SourcePath string    `json:"source_path"` // originating source file (empty for overview)
	SourceHash string    `json:"source_hash"` // hash of source used; enables incremental regen
	Generated  time.Time `json:"generated"`
}

// Manifest is the docs-index.json written into a repo's krabby-docs/ dir.
type Manifest struct {
	Repo      string    `json:"repo"`
	Model     string    `json:"model"`
	Generated time.Time `json:"generated"`
	Docs      []DocMeta `json:"docs"`
}

// ManifestName is the manifest filename inside the docs dir.
const ManifestName = "docs-index.json"

// Generator produces markdown docs for a repo clone.
type Generator interface {
	// Generate (re)builds docs for the repo at clonePath, writing markdown +
	// manifest into docsDir. It returns the manifest it wrote.
	Generate(ctx context.Context, repo, clonePath, docsDir string) (*Manifest, error)
}

// llmGenerator is the default LLM-backed generator.
type llmGenerator struct {
	cfg config.Docs
	llm *llm.Client
}

// New builds the default generator. The llm client is required; pass a
// configured *llm.Client. When docs are disabled callers should skip New.
func New(cfg config.Docs, chat *llm.Client) Generator {
	return &llmGenerator{cfg: cfg, llm: chat}
}

func (g *llmGenerator) Generate(_ context.Context, _ string, _ string, _ string) (*Manifest, error) {
	// TODO(scaffold):
	//   1. enumerate source files by Include/Exclude globs
	//   2. load graphify-out/graph.json for structural context (nodes/edges per file)
	//   3. for each changed file (source hash != manifest), prompt g.llm with
	//      {file content + graph neighborhood} to produce markdown; write it
	//   4. produce overview.md from god-nodes + communities
	//   5. write Manifest (ManifestName) into docsDir
	return nil, errors.New("docgen.Generate: not implemented (scaffold)")
}

// LoadManifest reads the manifest from a repo's docs dir. Returns (nil, nil)
// when no manifest exists yet.
func LoadManifest(docsDir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(docsDir, ManifestName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}

	return &m, nil
}
