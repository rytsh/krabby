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
documentation for an entire repository, from scratch. You are given dense
per-file summaries of the codebase and, when available, a knowledge-graph
overview (core abstractions and clusters). Write ONE complete, well-structured
Markdown document that explains the whole system meaningfully — what it is,
how it works and how the pieces fit together. Explain it to a colleague; do
not produce a file-by-file listing.

CRITICAL RULES:
- NEVER use placeholders such as "[to be documented]", "[example]", "[TBD]",
  "[tbd]" or similar. Omit the sentence or the whole section instead of stubbing
  it out.
- Extract REAL values only from the summaries, config and graph context below
  (names, paths, routes, HTTP methods, topic names, table/column names, env
  vars, config keys, types). Never invent behavior the input does not support.
- Write deep integration docs with real request/response samples taken verbatim
  from the input when the input shows request/response shapes.
- Do NOT use raw HTML tags in the Markdown; use standard Markdown syntax only.
- Mermaid: NO empty lines inside "subgraph ... end" blocks (blank lines there
  break the parser).
- Mermaid: node label text goes inside the brackets as plain text — do NOT put
  parentheses, quotes or other bracket characters inside a node label, because
  they break the parser. Write F[File System /share/*], NOT
  F[File System (/share/*)] and NOT F["File System (/share/*)"]. Rephrase the
  label to avoid () [] {} and quote characters entirely.
- Mermaid: keep every diagram small and syntactically valid, and label the edges.
- If the input contains VitePress/Vue-style markdown, keep spaces inside double
  curly braces in inline text (write { { value } } style with spaces) so the
  template interpolation is not triggered by accident.

Detect whether this is primarily a BACKEND service or a FRONTEND app
(Vue/React/Angular) from the summaries and graph, then follow the matching
section list. Skip any section the input has no evidence for rather than filling
it with guesses. Always start with a level-1 title naming the repository.

BACKEND sections (skip if no data):
1. Purpose — what the system does and the problem it solves.
2. External consumption — APIs called, messages/events consumed (Kafka, queues),
   databases and caches read, gRPC and external services depended on, with real
   endpoint paths, topic names and table names taken verbatim from the input.
3. External production — HTTP APIs exposed (real routes and methods, with a
   concrete curl example when request/response shapes are shown), messages/events
   produced, database writes, files or artifacts emitted.
4. Architecture diagram — one Mermaid flowchart (fenced block, "mermaid" tag)
   showing the components and the external systems they talk to.
5. Data flow — the one or two most important flows, as a Mermaid sequence or
   flow diagram plus a written explanation.
6. Configuration — real env vars, config keys and files (including Consul config
   when present in the input), and how they are used.
7. Database schema — tables/collections, key columns and relationships taken
   from migrations, models or schema definitions in the input.
8. Business logic — the non-trivial rules, validations, state machines and edge
   cases the code actually implements, in detail (a reader should be able to
   reason about behavior without opening the source).
9. Build & deployment — how to build, run and deploy, taken from the input.

FRONTEND sections (skip if no data):
1. Purpose & overview.
2. Application navigation graph — routes and how the user moves between them.
3. Module details — the main feature modules and their responsibilities.
4. API integrations by module — the backend endpoints each module calls.
5. Forms & validations — forms, fields and the validation rules applied.
6. Role-based access & guards — route/permission guards and the roles involved.
7. State management — stores, their shape and how state flows.
8. Component hierarchy — a Mermaid diagram of the key component tree.
9. Authentication & authorization — how the app authenticates and enforces access.
10. Key business capabilities — what the app lets users accomplish.
11. Technical architecture overview.
12. Navigation flow — a Mermaid diagram of the primary user journey.
13. Build & deployment.

Formatting rules:
- Output GitHub-flavored Markdown only. Do not wrap the whole response in a code fence.
- Prefer meaningful explanation over exhaustive enumeration; skip trivia.
- Mermaid labels may be wrapped in double quotes, for example A["Load config"].
  Never put another literal or escaped double quote inside an already quoted
  label. Rephrase the label instead of nesting or escaping quotes.`

// groupSummaryPrompt is the internal prompt for the grouped (per-community)
// summary phase. It summarizes several related files in one call, emitting one
// clearly delimited section per file. Summaries are intermediate material for
// the synthesis step, never shown to users directly.
const groupSummaryPrompt = `You are building an internal knowledge base that will later be
synthesized into whole-repository documentation. You are given several related
source files from one area of the codebase, plus optional knowledge-graph
context describing how they connect. Summarize EACH file accurately and densely.

For every file, in the same order given:
- Start its section with a level-2 heading that is exactly the file path
  (for example: ## internal/service/foo.go).
- Then, when present: the file's purpose and responsibility; key types and
  functions and their roles; dependencies and callers; HTTP endpoints served or
  called; message topics consumed or produced; databases, caches, files or
  external services read/written; notable side effects or gotchas.

Rules:
- Cover every file provided; do not merge files or skip any.
- Be factual and dense: roughly 120-250 words per file, bullet lists preferred.
- No overall introduction or conclusion, no code fences around the summaries.
- Do not invent behavior that is not supported by the code.
- Use exact names from the code (routes, topics, table/column names, env vars,
  function and type names) instead of paraphrasing them.
- Never use placeholders such as "[to be documented]" or "[example]"; omit a
  bullet entirely when the file gives no evidence for it.`

// Generator produces markdown docs for a repo clone.
type Generator interface {
	// Generate (re)builds docs for the repo at clonePath, writing markdown +
	// manifest into docsDir. It returns the manifest it wrote.
	Generate(ctx context.Context, repo, clonePath, docsDir string) (*Manifest, error)
}

// llmGenerator is the default LLM-backed generator.
type llmGenerator struct {
	cfg    config.Docs
	llm    *llm.Client // synthesis (final documentation.md)
	sum    *llm.Client // per-file summary phase (the bulk of the calls)
	engine *graphquery.Engine
}

// New builds the default generator. chat is required and produces the final
// synthesized documentation. summary is used for the per-file summary phase (the
// many grouped calls); pass a faster model here to speed up large builds. When
// summary is nil the chat client is used for both phases. engine may be nil, in
// which case summaries are generated from source content alone (without graph
// neighborhood context).
func New(cfg config.Docs, chat, summary *llm.Client, engine *graphquery.Engine) Generator {
	if summary == nil {
		summary = chat
	}

	return &llmGenerator{cfg: cfg, llm: chat, sum: summary, engine: engine}
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

	// Group related files so one LLM call summarizes a whole cluster instead of
	// one call per file. The graphify communities drive the grouping; files the
	// graph does not cover fall back to size-bounded batches.
	groups := g.buildGroups(files, graph)

	var (
		mu        sync.Mutex
		summaries []DocMeta
		regen     int
		sem       = make(chan struct{}, concurrency)
		wg        sync.WaitGroup
		genErr    error
	)

	for _, grp := range groups {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		wg.Add(1)
		sem <- struct{}{}

		go func(grp fileGroup) {
			defer wg.Done()
			defer func() { <-sem }()

			metas, groupRegen, err := g.summaryForGroup(ctx, clonePath, docsDir, grp, graph, prior)
			if err != nil {
				slog.Error("summarize group", "repo", repo, "community", grp.community, "files", len(grp.files), "error", err)

				mu.Lock()
				if genErr == nil {
					genErr = err
				}
				mu.Unlock()

				return
			}

			mu.Lock()
			summaries = append(summaries, metas...)
			regen += groupRegen
			mu.Unlock()
		}(grp)
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

// maxGroupBytes caps the total source bytes sent in a single grouped summary
// call. A community larger than this is split across several calls so one
// oversized cluster does not overflow the model context.
const maxGroupBytes = 96 * 1024

// fileGroup is a set of related files summarized together in one LLM call. A
// community < 0 marks an ungrouped/fallback batch (graph did not cover it).
type fileGroup struct {
	community int
	files     []string
}

// defaultMaxGroups bounds the grouped summary calls per run when the config
// leaves it unset. It keeps a highly fragmented graph (hundreds of tiny
// communities) from producing hundreds of LLM calls.
const defaultMaxGroups = 40

// maxFilesPerGroup keeps a single grouped call from covering an unreasonable
// number of files (which would dilute per-file summary quality) even when
// packing many small communities together.
const maxFilesPerGroup = 24

// buildGroups partitions files into clusters for grouped summarization. When a
// graph is available, files are grouped by their graphify community (majority
// vote per file); files the graph does not cover are batched by count. Without
// a graph, every file becomes its own group, preserving the original per-file
// behavior.
//
// To keep the LLM call count bounded regardless of how fragmented the graph is,
// the number of groups is capped: when there are more communities than the cap,
// small communities are packed together (largest communities keep their own
// group; the rest are bin-packed by file count) so a repo with 200 tiny
// communities still produces at most ~cap grouped calls.
func (g *llmGenerator) buildGroups(files []string, graph *graphquery.Graph) []fileGroup {
	if graph == nil {
		groups := make([]fileGroup, 0, len(files))
		for _, f := range files {
			groups = append(groups, fileGroup{community: -1, files: []string{f}})
		}

		return groups
	}

	selected := make(map[string]bool, len(files))
	for _, f := range files {
		selected[f] = true
	}

	byCommunity, ungrouped := graph.CommunityFiles()

	// Communities restricted to selected files, largest first (packing keeps big
	// clusters intact and merges the long tail of small ones).
	type community struct {
		id    int
		files []string
	}

	var comms []community
	for _, cid := range sortedCommunityIDs(byCommunity) {
		var members []string
		for _, f := range byCommunity[cid] {
			if selected[f] {
				members = append(members, f)
				delete(selected, f)
			}
		}
		if len(members) > 0 {
			sort.Strings(members)
			comms = append(comms, community{id: cid, files: members})
		}
	}

	// Files the graph does not attribute to a community, plus any selected file
	// the graph never mentioned, become one synthetic "ungrouped" community that
	// packing can split/merge like any other.
	var leftover []string
	for _, f := range ungrouped {
		if selected[f] {
			leftover = append(leftover, f)
			delete(selected, f)
		}
	}
	for _, f := range files {
		if selected[f] {
			leftover = append(leftover, f)
		}
	}
	if len(leftover) > 0 {
		sort.Strings(leftover)
		comms = append(comms, community{id: -1, files: leftover})
	}

	maxGroups := g.cfg.MaxGroups
	if maxGroups <= 0 {
		maxGroups = defaultMaxGroups
	}

	// Under the cap: one group per community (best focus). A community larger
	// than maxFilesPerGroup is still split so no single call is overloaded.
	var groups []fileGroup
	if len(comms) <= maxGroups {
		for _, c := range comms {
			groups = append(groups, splitCommunity(c.id, c.files)...)
		}

		return groups
	}

	// Over the cap: bin-pack communities into at most maxGroups groups. Sort by
	// size descending so large clusters anchor groups and small ones fill gaps.
	sort.SliceStable(comms, func(i, j int) bool { return len(comms[i].files) > len(comms[j].files) })

	total := 0
	for _, c := range comms {
		total += len(c.files)
	}

	// Target files-per-group so the packed groups spread evenly, but never more
	// than maxFilesPerGroup.
	target := (total + maxGroups - 1) / maxGroups
	if target < 1 {
		target = 1
	}
	if target > maxFilesPerGroup {
		target = maxFilesPerGroup
	}

	var (
		cur     []string
		curComm = -1
		first   = true
	)
	flush := func() {
		if len(cur) > 0 {
			sort.Strings(cur)
			groups = append(groups, fileGroup{community: curComm, files: cur})
			cur = nil
			first = true
		}
	}

	for _, c := range comms {
		for _, f := range c.files {
			if len(cur) >= target {
				flush()
			}
			if first {
				// A group keeps the community id only when it holds a single
				// community's files, so its graph context stays accurate.
				curComm = c.id
				first = false
			} else if curComm != c.id {
				curComm = -1 // mixed communities: no single-community context
			}
			cur = append(cur, f)
		}
	}
	flush()

	return groups
}

// splitCommunity breaks a community's files into groups no larger than
// maxFilesPerGroup, preserving the community id on each part.
func splitCommunity(cid int, files []string) []fileGroup {
	if len(files) <= maxFilesPerGroup {
		return []fileGroup{{community: cid, files: files}}
	}

	var groups []fileGroup
	for start := 0; start < len(files); start += maxFilesPerGroup {
		end := min(start+maxFilesPerGroup, len(files))
		part := make([]string, end-start)
		copy(part, files[start:end])
		groups = append(groups, fileGroup{community: cid, files: part})
	}

	return groups
}

// summaryForGroup produces (or reuses) the internal summaries for every file in
// a group. Unchanged files reuse their cached .sum; the remaining files are
// summarized together in a single LLM call. regen is the number of files
// actually re-summarized this run.
func (g *llmGenerator) summaryForGroup(
	ctx context.Context,
	clonePath, docsDir string,
	grp fileGroup,
	graph *graphquery.Graph,
	prior map[string]DocMeta,
) (metas []DocMeta, regen int, err error) {
	type pending struct {
		rel     string
		content string
		hash    string
	}

	var todo []pending

	for _, rel := range grp.files {
		fc, rerr := repofs.ReadFile(clonePath, rel, 0, maxSourceBytes)
		if rerr != nil {
			return nil, 0, rerr
		}

		hash := hashString(fc.Content)

		if meta, ok := reuseCachedSummary(docsDir, rel, hash, prior); ok {
			metas = append(metas, *meta)

			continue
		}

		todo = append(todo, pending{rel: rel, content: fc.Content, hash: hash})
	}

	if len(todo) == 0 {
		return metas, 0, nil
	}

	// Build one prompt covering every changed file in the group. Each file is
	// delimited so the model returns clearly separable per-file summaries.
	var user strings.Builder
	if graph != nil {
		var gctx string
		if grp.community >= 0 {
			gctx = graph.CommunityContext(grp.community, grp.files, 0)
		}
		if gctx != "" {
			user.WriteString("Knowledge-graph context for this cluster:\n")
			user.WriteString(gctx)
			user.WriteString("\n\n")
		}
	}

	user.WriteString("Summarize each of the following files. For every file, ")
	user.WriteString("start a section with a level-2 heading that is exactly the file path.\n\n")

	budget := maxGroupBytes / max(len(todo), 1)
	if budget < 2000 {
		budget = 2000
	}

	for _, p := range todo {
		content := p.content
		if len(content) > budget {
			content = content[:budget] + "\n(truncated)"
		}
		fmt.Fprintf(&user, "===== FILE: %s =====\n```\n%s\n```\n\n", p.rel, content)
	}

	out, err := g.sum.Complete(ctx, []llm.Message{
		{Role: "system", Content: groupSummaryPrompt},
		{Role: "user", Content: user.String()},
	})
	if err != nil {
		return nil, 0, err
	}

	todoPaths := make([]string, len(todo))
	for i, p := range todo {
		todoPaths[i] = p.rel
	}

	sections := splitSummarySections(out, todoPaths)

	now := time.Now()
	for _, p := range todo {
		body, ok := sections[p.rel]
		if !ok || strings.TrimSpace(body) == "" {
			// The model omitted this file; record a minimal placeholder so the
			// cache still advances and synthesis has something to reference.
			body = fmt.Sprintf("## %s\n\n(No summary produced for this file.)", p.rel)
		}

		sumRel := summaryPath(p.rel)
		sumAbs := filepath.Join(docsDir, filepath.FromSlash(sumRel))
		if werr := writeFileMkdir(sumAbs, []byte(strings.TrimSpace(body)+"\n")); werr != nil {
			return nil, 0, werr
		}

		metas = append(metas, DocMeta{
			Path:       sumRel,
			Title:      p.rel,
			SourcePath: p.rel,
			SourceHash: p.hash,
			Generated:  now,
		})
		regen++
	}

	return metas, regen, nil
}

// reuseCachedSummary returns the cached summary meta for rel when the prior run
// summarized the same source hash. Older manifests stored per-file docs at
// "<rel>.md"; their content is migrated into the .sum cache so an upgrade does
// not re-summarize the whole repo.
func reuseCachedSummary(docsDir, rel, hash string, prior map[string]DocMeta) (*DocMeta, bool) {
	p, ok := prior[rel]
	if !ok || p.SourceHash != hash {
		return nil, false
	}

	sumRel := summaryPath(rel)
	sumAbs := filepath.Join(docsDir, filepath.FromSlash(sumRel))
	oldAbs := filepath.Join(docsDir, filepath.FromSlash(p.Path))

	b, rerr := os.ReadFile(oldAbs)
	if rerr != nil {
		return nil, false
	}

	if p.Path != sumRel {
		if err := writeFileMkdir(sumAbs, b); err != nil {
			return nil, false
		}
	}

	return &DocMeta{
		Path:       sumRel,
		Title:      rel,
		SourcePath: rel,
		SourceHash: hash,
		Generated:  p.Generated,
	}, true
}

// splitSummarySections parses a grouped-summary response into per-file bodies.
// The prompt asks the model to head each file's section with "## <path>", and we
// also accept our own "===== FILE: <path> =====" delimiter as a fallback. Any
// text before the first recognized file heading is discarded.
func splitSummarySections(out string, files []string) map[string]string {
	want := make(map[string]bool, len(files))
	for _, f := range files {
		want[f] = true
	}

	sections := map[string]string{}

	var (
		current string
		buf     strings.Builder
	)

	flush := func() {
		if current != "" {
			sections[current] = strings.TrimSpace(buf.String())
		}
		buf.Reset()
	}

	for _, line := range strings.Split(out, "\n") {
		if p, ok := parseFileHeading(line, want); ok {
			flush()
			current = p
			// Keep a normalized heading so the summary still starts with the path.
			buf.WriteString("## " + p + "\n")

			continue
		}

		if current != "" {
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}
	flush()

	return sections
}

// parseFileHeading returns the file path when line is a section heading for one
// of the wanted files, matching either a Markdown "## <path>" heading or the
// "===== FILE: <path> =====" delimiter.
func parseFileHeading(line string, want map[string]bool) (string, bool) {
	trimmed := strings.TrimSpace(line)

	if strings.HasPrefix(trimmed, "=====") && strings.Contains(trimmed, "FILE:") {
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "====="))
		rest = strings.TrimSpace(strings.TrimSuffix(rest, "====="))
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "FILE:"))
		if want[rest] {
			return rest, true
		}
	}

	if strings.HasPrefix(trimmed, "#") {
		heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		heading = strings.Trim(heading, "`") // strip code-formatted paths
		if want[heading] {
			return heading, true
		}
	}

	return "", false
}

// sortedCommunityIDs returns community ids in ascending order for deterministic
// group ordering.
func sortedCommunityIDs(byCommunity map[int][]string) []int {
	ids := make([]int, 0, len(byCommunity))
	for id := range byCommunity {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	return ids
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

		// When the user has not pinned an explicit Include set, skip test
		// files, fixtures, mocks and dependency/noise directories by
		// default. Documentation summarises what the system does, not its
		// tests; including them bloats every per-file summary call and the
		// final synthesis payload (a common cause of synthesis timeouts).
		if len(g.cfg.Include) == 0 && isDocNoise(e.Path) {
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
		return defaultIncludeExts[strings.ToLower(path.Ext(rel))] ||
			defaultIncludeNames[strings.ToLower(path.Base(rel))]
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

// defaultIncludeNames is the allowlist of extensionless or dotted-suffix source
// files (matched by base name, case-insensitively) documented when no Include
// globs are configured. path.Ext does not classify these usefully — e.g.
// path.Ext("go.mod") is ".mod" — so they would otherwise be skipped.
var defaultIncludeNames = map[string]bool{
	"go.mod":           true,
	"go.sum":           true,
	"dockerfile":       true,
	"makefile":         true,
	"gemfile":          true,
	"rakefile":         true,
	"cargo.toml":       true,
	"cargo.lock":       true,
	"package.json":     true,
	"pyproject.toml":   true,
	"requirements.txt": true,
}

// docNoiseDirs are path segments whose subtrees carry no documentation value
// (tests fixtures, generated mocks, vendored/third-party code, build output).
// They are skipped by default so docs focus on the system's own behaviour.
var docNoiseDirs = map[string]bool{
	"testdata":     true,
	"test":         true,
	"tests":        true,
	"mock":         true,
	"mocks":        true,
	"__mocks__":    true,
	"fixtures":     true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".git":         true,
	"graphify-out": true,
	"krabby-docs":  true,
}

// isDocNoise reports whether a repo-relative path should be excluded from
// documentation by default: any file under a noise directory, or a test file
// (Go *_test.go, JS/TS *.test.* / *.spec.*, Python test_*.py / *_test.py).
func isDocNoise(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		if docNoiseDirs[strings.ToLower(seg)] {
			return true
		}
	}

	return isTestFileName(strings.ToLower(path.Base(rel)))
}

// isTestFileName recognises common test-file naming conventions across the
// languages in defaultIncludeExts.
func isTestFileName(base string) bool {
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
		return true
	case strings.HasSuffix(base, "_test.py"):
		return true
	}

	// JS/TS: name.test.ext or name.spec.ext for common extensions.
	for _, ext := range []string{".js", ".jsx", ".ts", ".tsx"} {
		if strings.HasSuffix(base, ".test"+ext) || strings.HasSuffix(base, ".spec"+ext) {
			return true
		}
	}

	// Java/Kotlin/C#/Scala convention: FooTest.ext / FooTests.ext.
	for _, ext := range []string{".java", ".kt", ".cs", ".scala"} {
		if strings.HasSuffix(base, ext) {
			stem := strings.TrimSuffix(base, ext)
			if strings.HasSuffix(stem, "test") || strings.HasSuffix(stem, "tests") {
				return true
			}
		}
	}

	return false
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
