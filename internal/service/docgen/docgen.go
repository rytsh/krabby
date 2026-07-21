// Package docgen turns a tracked repository into human-readable markdown
// documentation. The default generator prompts an LLM per file using the
// graphify graph neighborhood plus source content, and writes markdown under
// krabby-docs/ alongside a docs-index.json manifest.
//
// Generation is incremental: each source file's hash is recorded in the
// manifest, so unchanged files reuse their existing markdown on the next run.
package docgen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/repofs"
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

// OverviewName is the repo overview document filename.
const OverviewName = "overview.md"

// maxSourceBytes caps how much of a source file is sent to the LLM.
const maxSourceBytes = 48 * 1024

// DefaultPrompt is the built-in system prompt for per-file documentation. It is
// used whenever config.Docs.Prompt is empty, and is exported so the UI/config
// can show it as the effective default.
const DefaultPrompt = `You are a senior software engineer writing developer documentation.
Given a source file and its knowledge-graph neighborhood (the symbols it defines
and how they connect to the rest of the codebase), write concise, accurate
Markdown documentation for the file.

Cover:
- A one-paragraph summary of the file's purpose and responsibility.
- The key types, functions, and their roles.
- Important relationships to other parts of the codebase (callers, dependencies).
- Any notable behavior, side effects, or gotchas evident from the code.

Rules:
- Output GitHub-flavored Markdown only. Do not wrap the whole response in a code fence.
- Start with a level-1 heading naming the file.
- Be precise; do not invent behavior that is not supported by the code or graph.
- Keep it focused and skimmable. Prefer short paragraphs and bullet lists.`

// overviewPrompt is used for the repo-level overview document.
const overviewPrompt = `You are a senior software engineer writing a high-level
architecture overview for a codebase. Given knowledge-graph statistics, the most
connected core abstractions, and the largest clusters of related symbols, write a
concise Markdown overview that helps a new engineer orient quickly.

Cover the system's purpose (as far as it can be inferred), the core abstractions
and how they relate, and the main functional areas (clusters). Output
GitHub-flavored Markdown only, starting with a level-1 heading. Be precise and do
not invent specifics that are not supported by the provided data.`

// Generator produces markdown docs for a repo clone.
type Generator interface {
	// Generate (re)builds docs for the repo at clonePath, writing markdown +
	// manifest into docsDir. It returns the manifest it wrote.
	Generate(ctx context.Context, repo, clonePath, docsDir string) (*Manifest, error)
}

// llmGenerator is the default LLM-backed generator.
type llmGenerator struct {
	cfg    config.Docs
	llm    *llm.Client
	engine *graphquery.Engine
}

// New builds the default generator. The llm client is required. engine may be
// nil, in which case per-file docs are generated from source content alone
// (without graph neighborhood context).
func New(cfg config.Docs, chat *llm.Client, engine *graphquery.Engine) Generator {
	return &llmGenerator{cfg: cfg, llm: chat, engine: engine}
}

// Generate implements the incremental documentation pipeline.
func (g *llmGenerator) Generate(ctx context.Context, repo, clonePath, docsDir string) (*Manifest, error) {
	files, err := g.selectFiles(clonePath)
	if err != nil {
		return nil, fmt.Errorf("select source files; %w", err)
	}

	prior := loadPriorHashes(docsDir)

	graph := g.loadGraph(clonePath)

	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir docs dir; %w", err)
	}

	concurrency := g.cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	var (
		mu     sync.Mutex
		docs   []DocMeta
		sem    = make(chan struct{}, concurrency)
		wg     sync.WaitGroup
		genErr error
	)

	for _, rel := range files {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		wg.Add(1)
		sem <- struct{}{}

		go func(rel string) {
			defer wg.Done()
			defer func() { <-sem }()

			meta, err := g.docForFile(ctx, clonePath, docsDir, rel, graph, prior)
			if err != nil {
				slog.Error("generate doc for file", "repo", repo, "file", rel, "error", err)

				mu.Lock()
				if genErr == nil {
					genErr = err
				}
				mu.Unlock()

				return
			}

			if meta != nil {
				mu.Lock()
				docs = append(docs, *meta)
				mu.Unlock()
			}
		}(rel)
	}

	wg.Wait()

	// Overview document (best-effort; failure does not abort the run).
	if graph != nil {
		if meta, err := g.overviewDoc(ctx, docsDir, graph); err != nil {
			slog.Warn("generate overview doc", "repo", repo, "error", err)
		} else if meta != nil {
			docs = append(docs, *meta)
		}
	}

	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })

	man := &Manifest{
		Repo:      repo,
		Model:     g.llm.Model(),
		Generated: time.Now(),
		Docs:      docs,
	}

	if err := writeManifest(docsDir, man); err != nil {
		return nil, fmt.Errorf("write manifest; %w", err)
	}

	// If any per-file generation failed, surface it after writing what we have.
	if genErr != nil {
		return man, fmt.Errorf("one or more files failed to generate; first error: %w", genErr)
	}

	return man, nil
}

// docForFile generates (or reuses) the doc for one source file. It returns nil
// only when the file is skipped without producing metadata (should not happen).
func (g *llmGenerator) docForFile(
	ctx context.Context,
	clonePath, docsDir, rel string,
	graph *graphquery.Graph,
	prior map[string]DocMeta,
) (*DocMeta, error) {
	fc, err := repofs.ReadFile(clonePath, rel, 0, maxSourceBytes)
	if err != nil {
		return nil, err
	}

	hash := hashString(fc.Content)
	docRel := rel + ".md"
	docAbs := filepath.Join(docsDir, filepath.FromSlash(docRel))

	// Incremental: reuse existing markdown when the source hash is unchanged and
	// the markdown file still exists on disk.
	if p, ok := prior[docRel]; ok && p.SourceHash == hash {
		if _, statErr := os.Stat(docAbs); statErr == nil {
			p.Path = docRel

			return &p, nil
		}
	}

	var graphCtx string
	if graph != nil {
		graphCtx = graph.FileContext(rel, 0)
	}

	markdown, err := g.generateMarkdown(ctx, rel, fc.Content, graphCtx)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(docAbs), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir doc dir; %w", err)
	}

	if err := os.WriteFile(docAbs, []byte(markdown), 0o644); err != nil { //nolint:gosec // docs are non-secret
		return nil, fmt.Errorf("write doc %s; %w", docRel, err)
	}

	return &DocMeta{
		Path:       docRel,
		Title:      rel,
		SourcePath: rel,
		SourceHash: hash,
		Generated:  time.Now(),
	}, nil
}

// generateMarkdown prompts the LLM for one file's documentation.
func (g *llmGenerator) generateMarkdown(ctx context.Context, rel, content, graphCtx string) (string, error) {
	system := g.cfg.Prompt
	if strings.TrimSpace(system) == "" {
		system = DefaultPrompt
	}

	var user strings.Builder
	fmt.Fprintf(&user, "File: %s\n\n", rel)
	if graphCtx != "" {
		user.WriteString("Knowledge-graph neighborhood:\n")
		user.WriteString(graphCtx)
		user.WriteString("\n\n")
	}
	user.WriteString("Source:\n```\n")
	user.WriteString(content)
	user.WriteString("\n```\n")

	out, err := g.llm.Complete(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user.String()},
	})
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(out) + "\n", nil
}

// overviewDoc generates the repo-level overview from graph structure.
func (g *llmGenerator) overviewDoc(ctx context.Context, docsDir string, graph *graphquery.Graph) (*DocMeta, error) {
	overviewCtx := graph.OverviewContext(0, 0)

	out, err := g.llm.Complete(ctx, []llm.Message{
		{Role: "system", Content: overviewPrompt},
		{Role: "user", Content: "Knowledge-graph summary:\n\n" + overviewCtx},
	})
	if err != nil {
		return nil, err
	}

	markdown := strings.TrimSpace(out) + "\n"
	if err := os.WriteFile(filepath.Join(docsDir, OverviewName), []byte(markdown), 0o644); err != nil { //nolint:gosec // docs are non-secret
		return nil, fmt.Errorf("write overview; %w", err)
	}

	return &DocMeta{
		Path:      OverviewName,
		Title:     "Overview",
		Generated: time.Now(),
	}, nil
}

// selectFiles walks the clone and returns repo-relative slash paths that match
// the Include globs and none of the Exclude globs. When Include is empty a set of
// sensible source extensions is documented. graphify-out and .git are skipped by
// repofs.ListFiles.
func (g *llmGenerator) selectFiles(clonePath string) ([]string, error) {
	entries, err := repofs.ListFiles(clonePath, "", true)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, e := range entries {
		if e.IsDir {
			continue
		}

		// Never document our own output.
		if strings.HasPrefix(e.Path, "krabby-docs/") {
			continue
		}

		if !g.matchInclude(e.Path) {
			continue
		}

		if g.matchExclude(e.Path) {
			continue
		}

		out = append(out, e.Path)
	}

	sort.Strings(out)

	return out, nil
}

func (g *llmGenerator) matchInclude(rel string) bool {
	if len(g.cfg.Include) == 0 {
		return defaultIncludeExts[strings.ToLower(path.Ext(rel))]
	}

	return matchAny(g.cfg.Include, rel)
}

func (g *llmGenerator) matchExclude(rel string) bool {
	return matchAny(g.cfg.Exclude, rel)
}

// matchAny reports whether rel matches any glob. A glob is matched against both
// the full path and the base name, and a bare directory prefix (e.g. "vendor/")
// matches everything under it.
func matchAny(globs []string, rel string) bool {
	base := path.Base(rel)
	for _, gl := range globs {
		if gl == "" {
			continue
		}

		if strings.HasSuffix(gl, "/") && strings.HasPrefix(rel, gl) {
			return true
		}

		if ok, _ := path.Match(gl, rel); ok {
			return true
		}

		if ok, _ := path.Match(gl, base); ok {
			return true
		}
	}

	return false
}

// defaultIncludeExts is the source-file allowlist used when no Include globs are
// configured.
var defaultIncludeExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".java": true, ".kt": true, ".rb": true, ".rs": true, ".c": true, ".h": true,
	".cc": true, ".cpp": true, ".hpp": true, ".cs": true, ".php": true, ".swift": true,
	".scala": true, ".m": true, ".mm": true, ".sh": true, ".sql": true,
}

// loadGraph loads the repo's graph via the engine; returns nil when unavailable.
func (g *llmGenerator) loadGraph(clonePath string) *graphquery.Graph {
	if g.engine == nil {
		return nil
	}

	graphPath := graphify.GraphPath(clonePath)
	graph, err := g.engine.Graph(graphPath)
	if err != nil {
		slog.Warn("docgen: graph unavailable, generating without graph context", "path", graphPath, "error", err)

		return nil
	}

	return graph
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

// loadPriorHashes returns the previous run's docs keyed by doc path, for
// incremental regeneration. Missing/corrupt manifests yield an empty map.
func loadPriorHashes(docsDir string) map[string]DocMeta {
	out := map[string]DocMeta{}
	man, err := LoadManifest(docsDir)
	if err != nil || man == nil {
		return out
	}

	for _, d := range man.Docs {
		out[d.Path] = d
	}

	return out
}

func writeManifest(docsDir string, man *Manifest) error {
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(docsDir, ManifestName), b, 0o644) //nolint:gosec // manifest is non-secret
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}
