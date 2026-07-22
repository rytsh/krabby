// Package docgen turns a tracked repository into human-readable markdown
// documentation. The default generator works in two phases: it first builds a
// dense per-file summary for every source file (cached under the external docs
// directory's .summaries/ and regenerated only when the source hash changes),
// then synthesizes ONE comprehensive documentation.md for the whole repository
// from those summaries plus the knowledge-graph overview.
//
// Only documentation.md is listed in the manifest and indexed for RAG; the
// summaries are an internal cache (stored without a .md extension so the docs
// indexer ignores them).
package docgen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
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

// DocMeta describes one generated markdown document (or an internal summary).
type DocMeta struct {
	Path       string    `json:"path"`        // path relative to the repo's docs directory
	Title      string    `json:"title"`       // human title
	SourcePath string    `json:"source_path"` // originating source file (empty for the synthesized doc)
	SourceHash string    `json:"source_hash"` // hash of source used; enables incremental regen
	Generated  time.Time `json:"generated"`
}

// Manifest is the docs-index.json written into a repo's external docs directory.
type Manifest struct {
	Repo      string    `json:"repo"`
	Model     string    `json:"model"`
	Generated time.Time `json:"generated"`
	// Docs are the user-facing documents (the synthesized documentation.md).
	Docs []DocMeta `json:"docs"`
	// Summaries is the internal per-file summary cache used for incremental
	// regeneration and as synthesis input. Not exposed by ListDocs.
	Summaries []DocMeta `json:"summaries,omitempty"`

	// ChangedDocs reports whether this run rewrote any user-facing document.
	// In-memory only (not persisted): callers use it to skip re-indexing when
	// documentation is unchanged.
	ChangedDocs bool `json:"-"`
}

// ManifestName is the manifest filename inside the docs dir.
const ManifestName = "docs-index.json"

// DocName is the single comprehensive documentation file.
const DocName = "documentation.md"

// summariesDir holds the internal per-file summary cache inside the docs dir.
// Files use a .sum extension so the RAG doc indexer (which walks *.md) skips them.
const summariesDir = ".summaries"

// maxSourceBytes caps how much of a source file is sent to the LLM.
const maxSourceBytes = 48 * 1024

// maxSynthesisBytes caps the total summary input for the synthesis call. When
// exceeded, each summary is truncated to an equal share.
const maxSynthesisBytes = 256 * 1024

// DefaultPrompt is the built-in system prompt for the final synthesis of the
// comprehensive repository documentation. It is used whenever
// config.Docs.Prompt is empty, and is exported so the UI/config can show it as
// the effective default.
const DefaultPrompt = `You are a senior software engineer writing comprehensive developer
documentation for an entire repository. You are given dense per-file summaries
of the codebase and, when available, a knowledge-graph overview (core
abstractions and clusters). Write ONE complete, well-structured Markdown
document that explains the whole system meaningfully — what it is, how it works
and how the pieces fit together. Explain it to a colleague; do not produce a
file-by-file listing.

Structure (adapt to the codebase):
- Level-1 title naming the repository.
- Purpose: what the system does and the problem it solves.
- Architecture: the main components/modules, their responsibilities and how
  they interact. Include one Mermaid architecture diagram (a fenced code block
  with the "mermaid" language tag, flowchart syntax) showing the components and
  the external systems they talk to.
- How it works end to end: the most important flows (request handling, message
  processing, data pipelines, background jobs). Use a small Mermaid sequence or
  flow diagram for the one or two most important flows.
- Integrations and I/O: HTTP APIs served and called (routes, where requests
  go), message topics (Kafka etc.) consumed/produced, databases, caches, files
  and external services — what comes in, what goes out, and where it goes.
- Configuration and operations, when evident: env vars, config files, how to
  run and deploy.
- Notable behaviors, invariants and gotchas.

Rules:
- Output GitHub-flavored Markdown only. Do not wrap the whole response in a code fence.
- Be precise; do not invent behavior that is not supported by the input.
- Prefer meaningful explanation over exhaustive enumeration; skip trivia.
- Keep every Mermaid diagram small and syntactically valid, and label the edges.
- Mermaid labels may be wrapped in double quotes, for example A["Load config"].
  Never put another literal or escaped double quote inside an already quoted
  label. Rephrase the label instead of nesting or escaping quotes.`

// summaryPrompt is the fixed internal prompt for the per-file summary phase.
// Summaries are intermediate material for the synthesis step, never shown to
// users directly.
const summaryPrompt = `You are building an internal knowledge base that will later be
synthesized into whole-repository documentation. Summarize the given source
file accurately and densely.

Include, when present: the file's purpose and responsibility; key types and
functions and their roles; dependencies and callers; HTTP endpoints served or
called; message topics (Kafka etc.) consumed or produced; databases, caches,
files or external services read/written; notable side effects or gotchas.

Rules:
- Output Markdown starting with a level-2 heading that is exactly the file path.
- Be factual and dense: roughly 150-300 words, bullet lists preferred.
- No introductions, conclusions or code fences.
- Do not invent behavior that is not supported by the code.`

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
// nil, in which case summaries are generated from source content alone
// (without graph neighborhood context).
func New(cfg config.Docs, chat *llm.Client, engine *graphquery.Engine) Generator {
	return &llmGenerator{cfg: cfg, llm: chat, engine: engine}
}

// Generate implements the two-phase pipeline: incremental per-file summaries,
// then one comprehensive documentation.md synthesized from them.
func (g *llmGenerator) Generate(ctx context.Context, repo, clonePath, docsDir string) (*Manifest, error) {
	files, err := g.selectFiles(clonePath)
	if err != nil {
		return nil, fmt.Errorf("select source files; %w", err)
	}

	priorMan, _ := LoadManifest(docsDir)
	prior := priorSummaries(priorMan)

	graph := g.loadGraph(clonePath)

	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir docs dir; %w", err)
	}

	concurrency := g.cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 8
	}

	var (
		mu        sync.Mutex
		summaries []DocMeta
		regen     int
		sem       = make(chan struct{}, concurrency)
		wg        sync.WaitGroup
		genErr    error
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

			meta, reused, err := g.summaryForFile(ctx, clonePath, docsDir, rel, graph, prior)
			if err != nil {
				slog.Error("summarize file", "repo", repo, "file", rel, "error", err)

				mu.Lock()
				if genErr == nil {
					genErr = err
				}
				mu.Unlock()

				return
			}

			mu.Lock()
			summaries = append(summaries, *meta)
			if !reused {
				regen++
			}
			mu.Unlock()
		}(rel)
	}

	wg.Wait()

	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Path < summaries[j].Path })

	// Synthesis: skip the LLM call when nothing changed and the doc exists.
	docMeta, synthesized, synthErr := g.maybeSynthesize(ctx, repo, docsDir, graph, summaries, priorMan, regen)

	var docs []DocMeta
	if docMeta != nil {
		docs = append(docs, *docMeta)
	}

	man := &Manifest{
		Repo:        repo,
		Model:       g.llm.Model(),
		Generated:   time.Now(),
		Docs:        docs,
		Summaries:   summaries,
		ChangedDocs: synthesized,
	}

	if err := writeManifest(docsDir, man); err != nil {
		return nil, fmt.Errorf("write manifest; %w", err)
	}

	// Drop stale user-facing markdown (old per-file docs, old overview.md).
	cleanupStaleDocs(docsDir, docs)

	if synthErr != nil {
		return man, fmt.Errorf("synthesize documentation; %w", synthErr)
	}

	if genErr != nil {
		return man, fmt.Errorf("one or more files failed to summarize; first error: %w", genErr)
	}

	return man, nil
}

// summaryPath returns the docs-dir-relative cache path for a source file.
func summaryPath(rel string) string {
	return path.Join(summariesDir, rel+".sum")
}

// summaryForFile generates (or reuses) the internal summary for one source
// file. reused reports whether the cached summary was still valid.
func (g *llmGenerator) summaryForFile(
	ctx context.Context,
	clonePath, docsDir, rel string,
	graph *graphquery.Graph,
	prior map[string]DocMeta,
) (meta *DocMeta, reused bool, err error) {
	fc, err := repofs.ReadFile(clonePath, rel, 0, maxSourceBytes)
	if err != nil {
		return nil, false, err
	}

	hash := hashString(fc.Content)
	sumRel := summaryPath(rel)
	sumAbs := filepath.Join(docsDir, filepath.FromSlash(sumRel))

	// Incremental: reuse the cached summary when the source hash is unchanged.
	// Older manifests stored per-file docs at "<rel>.md"; migrate their content
	// into the cache so an upgrade does not re-summarize the whole repo.
	if p, ok := prior[rel]; ok && p.SourceHash == hash {
		oldAbs := filepath.Join(docsDir, filepath.FromSlash(p.Path))
		if b, rerr := os.ReadFile(oldAbs); rerr == nil {
			if p.Path != sumRel {
				if err := writeFileMkdir(sumAbs, b); err != nil {
					return nil, false, err
				}
			}

			return &DocMeta{
				Path:       sumRel,
				Title:      rel,
				SourcePath: rel,
				SourceHash: hash,
				Generated:  p.Generated,
			}, true, nil
		}
	}

	var graphCtx string
	if graph != nil {
		graphCtx = graph.FileContext(rel, 0)
	}

	var user strings.Builder
	fmt.Fprintf(&user, "File: %s\n\n", rel)
	if graphCtx != "" {
		user.WriteString("Knowledge-graph neighborhood:\n")
		user.WriteString(graphCtx)
		user.WriteString("\n\n")
	}
	user.WriteString("Source:\n```\n")
	user.WriteString(fc.Content)
	user.WriteString("\n```\n")

	out, err := g.llm.Complete(ctx, []llm.Message{
		{Role: "system", Content: summaryPrompt},
		{Role: "user", Content: user.String()},
	})
	if err != nil {
		return nil, false, err
	}

	if err := writeFileMkdir(sumAbs, []byte(strings.TrimSpace(out)+"\n")); err != nil {
		return nil, false, err
	}

	return &DocMeta{
		Path:       sumRel,
		Title:      rel,
		SourcePath: rel,
		SourceHash: hash,
		Generated:  time.Now(),
	}, false, nil
}

// maybeSynthesize produces documentation.md from the summaries, reusing the
// previous document when no summary changed and the file still exists. The
// second return reports whether a fresh document was written.
func (g *llmGenerator) maybeSynthesize(
	ctx context.Context,
	repo, docsDir string,
	graph *graphquery.Graph,
	summaries []DocMeta,
	priorMan *Manifest,
	regen int,
) (*DocMeta, bool, error) {
	docAbs := filepath.Join(docsDir, DocName)

	if regen == 0 && priorMan != nil {
		for _, d := range priorMan.Docs {
			if d.Path != DocName {
				continue
			}

			if _, err := os.Stat(docAbs); err == nil && len(priorMan.Summaries) == len(summaries) {
				return &d, false, nil
			}
		}
	}

	if len(summaries) == 0 {
		return nil, false, fmt.Errorf("no source files to document")
	}

	system := g.cfg.Prompt
	if strings.TrimSpace(system) == "" {
		system = DefaultPrompt
	}

	var user strings.Builder
	fmt.Fprintf(&user, "Repository: %s\n\n", repo)

	if graph != nil {
		user.WriteString("Knowledge-graph overview:\n")
		user.WriteString(graph.OverviewContext(0, 0))
		user.WriteString("\n\n")
	}

	user.WriteString("Per-file summaries:\n\n")
	user.WriteString(joinSummaries(docsDir, summaries, maxSynthesisBytes))

	out, err := g.llm.Complete(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user.String()},
	})
	if err != nil {
		return nil, false, err
	}

	markdown := strings.TrimSpace(out) + "\n"
	if err := os.WriteFile(docAbs, []byte(markdown), 0o644); err != nil { //nolint:gosec // docs are non-secret
		return nil, false, fmt.Errorf("write %s; %w", DocName, err)
	}

	return &DocMeta{
		Path:      DocName,
		Title:     "Documentation",
		Generated: time.Now(),
	}, true, nil
}

// joinSummaries concatenates summary contents within a byte budget. When the
// total exceeds the budget every summary is truncated to an equal share so the
// synthesis still sees the whole repository.
func joinSummaries(docsDir string, summaries []DocMeta, budget int) string {
	contents := make([]string, 0, len(summaries))
	total := 0

	for _, s := range summaries {
		b, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(s.Path)))
		if err != nil {
			slog.Warn("read summary", "path", s.Path, "error", err)

			continue
		}

		c := strings.TrimSpace(string(b))
		contents = append(contents, c)
		total += len(c)
	}

	if total > budget && len(contents) > 0 {
		share := budget / len(contents)
		if share < 400 {
			share = 400
		}

		for i, c := range contents {
			if len(c) > share {
				contents[i] = c[:share] + "\n(truncated)"
			}
		}
	}

	return strings.Join(contents, "\n\n---\n\n")
}

// cleanupStaleDocs removes user-facing markdown files that are not referenced
// by the manifest (e.g. per-file docs and overview.md from older layouts). The
// internal summaries cache is left untouched; emptied directories are pruned.
func cleanupStaleDocs(docsDir string, docs []DocMeta) {
	keep := map[string]bool{}
	for _, d := range docs {
		keep[d.Path] = true
	}

	var dirs []string

	_ = filepath.WalkDir(docsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort cleanup
		}

		rel, rerr := filepath.Rel(docsDir, p)
		if rerr != nil {
			return nil //nolint:nilerr // best-effort cleanup
		}

		slashRel := filepath.ToSlash(rel)
		if slashRel == "." || slashRel == summariesDir || strings.HasPrefix(slashRel, summariesDir+"/") {
			if d.IsDir() && slashRel == summariesDir {
				return filepath.SkipDir
			}

			return nil
		}

		if d.IsDir() {
			dirs = append(dirs, p)

			return nil
		}

		if strings.HasSuffix(d.Name(), ".md") && !keep[slashRel] {
			if err := os.Remove(p); err != nil {
				slog.Warn("remove stale doc", "path", slashRel, "error", err)
			}
		}

		return nil
	})

	// Prune emptied directories bottom-up.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		_ = os.Remove(dir) // fails (kept) when non-empty
	}
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

// priorSummaries indexes the previous run's summary cache by source path. Docs
// entries with a SourcePath are included too so pre-synthesis (per-file)
// layouts keep their cache across the upgrade.
func priorSummaries(man *Manifest) map[string]DocMeta {
	out := map[string]DocMeta{}
	if man == nil {
		return out
	}

	for _, d := range man.Docs {
		if d.SourcePath != "" {
			out[d.SourcePath] = d
		}
	}

	for _, d := range man.Summaries {
		if d.SourcePath != "" {
			out[d.SourcePath] = d
		}
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

func writeFileMkdir(abs string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir %s; %w", filepath.Dir(abs), err)
	}

	if err := os.WriteFile(abs, b, 0o644); err != nil { //nolint:gosec // docs are non-secret
		return fmt.Errorf("write %s; %w", abs, err)
	}

	return nil
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}
