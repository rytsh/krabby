// Package coderag indexes raw source code into the vector store and retrieves
// matching snippets for natural-language (or code) queries.
//
// Unlike the docs RAG (file-level: whole markdown documents are returned), code
// retrieval is snippet-level: each hit carries the matching chunk plus its
// repo-relative path and line range, so a caller can read more context via
// read_file when needed.
//
// Chunking is symbol-aware: the graphify knowledge graph anchors symbols
// (functions, types, classes) to source lines, and chunk boundaries follow
// those anchors. Files absent from the graph fall back to line-aligned windows.
package coderag

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// Snippet is one code search hit.
type Snippet struct {
	Repo      string  `json:"repo"`
	Path      string  `json:"path"` // repo-relative source path
	Symbol    string  `json:"symbol,omitempty"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Line      int     `json:"line,omitempty"` // exact text match; semantic hits may omit it
	Score     float32 `json:"score"`
	Snippet   string  `json:"snippet"`
}

// Service indexes source code and retrieves matching snippets.
type Service struct {
	cfg    config.CodeRAG
	emb    *embedder.Client
	store  vectorstore.Store
	engine *graphquery.Engine
	text   *TextStore
}

const maxSearchResults = 100

// New builds the shared code indexing service. text provides normal BM25
// search; emb and store may be nil when semantic search is disabled. engine may
// be nil (chunking then always falls back to line windows).
func New(
	cfg config.CodeRAG,
	emb *embedder.Client,
	store vectorstore.Store,
	engine *graphquery.Engine,
	text *TextStore,
) *Service {
	return &Service{cfg: cfg, emb: emb, store: store, engine: engine, text: text}
}

// Index (re)builds the code vector index for a repo clone. It selects source
// files, chunks them (symbol-aware via the repo's graph when available), embeds
// the chunks and upserts them, replacing any prior vectors for the repo.
func (s *Service) Index(ctx context.Context, repo, clonePath string) error {
	items, fileCount, err := s.indexItems(clonePath, repo)
	if err != nil {
		return err
	}

	if s.text != nil {
		if err := s.text.ReplaceRepo(ctx, repo, items); err != nil {
			return fmt.Errorf("replace normal code index; %w", err)
		}
	}

	if s.emb == nil || s.store == nil {
		slog.Info("code text index rebuilt", "repo", repo, "files", fileCount, "chunks", len(items))
		return nil
	}

	if len(items) == 0 {
		return s.store.DeleteRepo(ctx, repo)
	}

	texts := make([]string, len(items))
	for i := range items {
		texts[i] = items[i].Payload.Chunk
	}

	vecs, err := s.emb.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed %d code chunks; %w", len(texts), err)
	}

	if len(vecs) != len(items) {
		return fmt.Errorf("embedder returned %d vectors for %d chunks", len(vecs), len(items))
	}

	for i := range items {
		items[i].Vector = vecs[i]
	}

	// Full rebuild: drop prior vectors so deleted files disappear, then upsert.
	if err := s.store.DeleteRepo(ctx, repo); err != nil {
		return fmt.Errorf("clear prior code vectors; %w", err)
	}

	if err := s.store.Upsert(ctx, items); err != nil {
		return fmt.Errorf("upsert %d code vectors; %w", len(items), err)
	}

	slog.Info("code indexes rebuilt", "repo", repo, "files", fileCount, "chunks", len(items))

	return nil
}

// IndexChanged incrementally updates the code indexes: only the given
// repo-relative paths are re-read, re-chunked and re-embedded, replacing their
// prior rows; paths that no longer exist (or fall outside the selection rules)
// just have their rows dropped. Unchanged files keep their existing FTS rows
// and vectors, so refreshes touching few files cost a fraction of a full Index.
func (s *Service) IndexChanged(ctx context.Context, repo, clonePath string, changed []string) error {
	if len(changed) == 0 {
		return nil
	}

	// Normalize to the repo-relative slash form used as DocPath in both stores.
	paths := make([]string, 0, len(changed))
	for _, rel := range changed {
		paths = append(paths, path.Clean(filepath.ToSlash(rel)))
	}

	var syms map[string][]symbol

	var (
		items     []vectorstore.Item
		reindexed int
	)

	for _, rel := range paths {
		if !s.selectedFile(clonePath, rel) {
			continue // deleted/excluded: rows are dropped below, nothing to add
		}

		fc, err := repofs.ReadFile(clonePath, rel, 0, 0)
		if err != nil {
			slog.Warn("coderag: skip unreadable file", "repo", repo, "file", rel, "error", err)

			continue
		}

		if fc.Truncated || strings.ContainsRune(fc.Content, '\x00') {
			continue
		}

		if syms == nil {
			syms = s.fileSymbols(clonePath)
		}

		for i, c := range chunkFile(fc.Content, syms[rel], s.cfg.ChunkSize, s.cfg.ChunkOverlap) {
			items = append(items, vectorstore.Item{
				ID: fmt.Sprintf("%s/%s#%d", repo, rel, i),
				Payload: vectorstore.Payload{
					Repo:      repo,
					DocPath:   rel,
					Symbol:    c.Symbol,
					StartLine: c.StartLine,
					EndLine:   c.EndLine,
					Chunk:     c.Text,
				},
			})
		}

		reindexed++
	}

	if s.text != nil {
		if err := s.text.DeletePaths(ctx, repo, paths); err != nil {
			return fmt.Errorf("drop changed code search chunks; %w", err)
		}

		if err := s.text.InsertItems(ctx, items); err != nil {
			return fmt.Errorf("insert changed code search chunks; %w", err)
		}
	}

	if s.emb == nil || s.store == nil {
		slog.Info("code text index updated", "repo", repo, "changed", len(changed), "files", reindexed, "chunks", len(items))

		return nil
	}

	if len(items) > 0 {
		texts := make([]string, len(items))
		for i := range items {
			texts[i] = items[i].Payload.Chunk
		}

		vecs, err := s.emb.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed %d code chunks; %w", len(texts), err)
		}

		if len(vecs) != len(items) {
			return fmt.Errorf("embedder returned %d vectors for %d chunks", len(vecs), len(items))
		}

		for i := range items {
			items[i].Vector = vecs[i]
		}
	}

	// Drop prior vectors of every changed path (stale chunk counts, deletions),
	// then upsert the fresh chunks.
	if err := s.store.DeletePaths(ctx, repo, paths); err != nil {
		return fmt.Errorf("drop changed code vectors; %w", err)
	}

	if len(items) > 0 {
		if err := s.store.Upsert(ctx, items); err != nil {
			return fmt.Errorf("upsert %d code vectors; %w", len(items), err)
		}
	}

	slog.Info("code indexes updated", "repo", repo, "changed", len(changed), "files", reindexed, "chunks", len(items))

	return nil
}

// selectedFile reports whether rel would be picked by selectFiles: every parent
// directory passes the skip rules and the file itself is a regular, size-capped,
// included and not excluded source file.
func (s *Service) selectedFile(clonePath, rel string) bool {
	prefix := ""
	for seg := range strings.SplitSeq(path.Dir(rel), "/") {
		if seg == "." || seg == "" {
			continue
		}

		prefix = path.Join(prefix, seg)
		if hardSkipDirs[seg] || (len(s.cfg.Include) == 0 && defaultNoiseDirs[seg]) || matchAny(s.cfg.Exclude, prefix+"/") {
			return false
		}
	}

	info, err := os.Lstat(filepath.Join(clonePath, filepath.FromSlash(rel)))
	if err != nil || !info.Mode().IsRegular() {
		return false
	}

	return info.Size() <= repofs.MaxFileBytes && s.matchInclude(rel) && !matchAny(s.cfg.Exclude, rel)
}

// IndexText builds only the local bw FTS index. It is used to bootstrap the
// normal search index for repositories tracked before this index was added.
func (s *Service) IndexText(ctx context.Context, repo, clonePath string) error {
	if s.text == nil {
		return nil
	}

	items, fileCount, err := s.indexItems(clonePath, repo)
	if err != nil {
		return err
	}
	if err := s.text.ReplaceRepo(ctx, repo, items); err != nil {
		return fmt.Errorf("replace normal code index; %w", err)
	}

	slog.Info("code text index rebuilt", "repo", repo, "files", fileCount, "chunks", len(items))
	return nil
}

func (s *Service) indexItems(clonePath, repo string) ([]vectorstore.Item, int, error) {
	files, err := s.selectFiles(clonePath)
	if err != nil {
		return nil, 0, fmt.Errorf("select source files; %w", err)
	}

	symsByFile := s.fileSymbols(clonePath)

	var items []vectorstore.Item

	for _, rel := range files {
		fc, err := repofs.ReadFile(clonePath, rel, 0, 0)
		if err != nil {
			slog.Warn("coderag: skip unreadable file", "repo", repo, "file", rel, "error", err)

			continue
		}

		if fc.Truncated || strings.ContainsRune(fc.Content, '\x00') {
			// Oversized or binary; not useful as embedded text.
			continue
		}

		for i, c := range chunkFile(fc.Content, symsByFile[rel], s.cfg.ChunkSize, s.cfg.ChunkOverlap) {
			items = append(items, vectorstore.Item{
				ID: fmt.Sprintf("%s/%s#%d", repo, rel, i),
				Payload: vectorstore.Payload{
					Repo:      repo,
					DocPath:   rel,
					Symbol:    c.Symbol,
					StartLine: c.StartLine,
					EndLine:   c.EndLine,
					Chunk:     c.Text,
				},
			})
		}
	}

	return items, len(files), nil
}

// DeleteRepo removes a repo's vectors from the code index (on repo removal).
func (s *Service) DeleteRepo(ctx context.Context, repo string) error {
	var errs []error
	if s.text != nil {
		if err := s.text.DeleteRepo(ctx, repo); err != nil {
			errs = append(errs, err)
		}
	}
	if s.store != nil {
		if err := s.store.DeleteRepo(ctx, repo); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Retrieve returns up to topK code snippets most relevant to the query.
// repo == "" searches across all repos. topK <= 0 uses the configured default.
func (s *Service) Retrieve(ctx context.Context, repo, query string, topK int) ([]Snippet, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query is empty")
	}

	if topK <= 0 {
		topK = s.cfg.TopK
	}

	if topK <= 0 {
		topK = 10
	}
	if topK > maxSearchResults {
		topK = maxSearchResults
	}

	vecs, err := s.emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query; %w", err)
	}

	if len(vecs) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors for the query", len(vecs))
	}

	// Fetch extra candidates because fallback chunks overlap and are deduplicated
	// below; this keeps the final result useful without returning near-identical
	// snippets from the same file region.
	matches, err := s.store.Search(ctx, vectorstore.FilterKey(repo), vecs[0], topK*3)
	if err != nil {
		return nil, fmt.Errorf("vector search; %w", err)
	}

	out := make([]Snippet, 0, len(matches))
	for _, m := range matches {
		candidate := Snippet{
			Repo:      m.Payload.Repo,
			Path:      m.Payload.DocPath,
			Symbol:    m.Payload.Symbol,
			StartLine: m.Payload.StartLine,
			EndLine:   m.Payload.EndLine,
			Score:     m.Score,
			Snippet:   m.Payload.Chunk,
		}
		if overlapsExisting(out, candidate) {
			continue
		}

		out = append(out, candidate)
		if len(out) == topK {
			break
		}
	}

	return out, nil
}

// overlapsExisting reports whether candidate overlaps at least half of the
// smaller snippet's line range with an already selected hit from the same file.
func overlapsExisting(selected []Snippet, candidate Snippet) bool {
	for _, prev := range selected {
		if prev.Repo != candidate.Repo || prev.Path != candidate.Path {
			continue
		}

		start := max(prev.StartLine, candidate.StartLine)
		end := min(prev.EndLine, candidate.EndLine)
		if end < start {
			continue
		}

		intersection := end - start + 1
		smaller := min(prev.EndLine-prev.StartLine+1, candidate.EndLine-candidate.StartLine+1)
		if smaller > 0 && intersection*2 >= smaller {
			return true
		}
	}

	return false
}

// fileSymbols loads the repo's graphify graph and groups its symbol nodes by
// repo-relative source file. Returns an empty map when the graph is missing.
func (s *Service) fileSymbols(clonePath string) map[string][]symbol {
	out := map[string][]symbol{}

	if s.engine == nil {
		return out
	}

	graph, err := s.engine.Graph(graphify.GraphPath(clonePath))
	if err != nil {
		slog.Warn("coderag: graph unavailable, using line-window chunking", "path", clonePath, "error", err)

		return out
	}

	for _, n := range graph.Nodes {
		if n.SourceFile == "" {
			continue
		}

		// Skip file-level nodes: the whole file is not one symbol.
		if n.Label == path.Base(n.SourceFile) {
			continue
		}

		line := parseLine(n.SourceLocation)
		if line == 0 {
			continue
		}

		rel := path.Clean(strings.TrimPrefix(n.SourceFile, "./"))
		out[rel] = append(out[rel], symbol{Name: n.Label, Line: line})
	}

	return out
}

// selectFiles walks the entire clone and returns repo-relative slash paths
// matching the Include globs and none of the Exclude globs. It intentionally
// does not use repofs.ListFiles because that API's 2,000-entry response cap is
// appropriate for UI/MCP listings but would silently truncate large indexes.
func (s *Service) selectFiles(clonePath string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(clonePath, func(filePath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if filePath == clonePath {
			return nil
		}

		rel, err := filepath.Rel(clonePath, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if hardSkipDirs[d.Name()] || (len(s.cfg.Include) == 0 && defaultNoiseDirs[d.Name()]) || matchAny(s.cfg.Exclude, rel+"/") {
				return fs.SkipDir
			}

			return nil
		}

		// Do not follow or read symlinks and non-regular filesystem entries.
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.Size() > repofs.MaxFileBytes || !s.matchInclude(rel) || matchAny(s.cfg.Exclude, rel) {
			return nil
		}

		out = append(out, rel)

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(out)

	return out, nil
}

func (s *Service) matchInclude(rel string) bool {
	if len(s.cfg.Include) == 0 {
		return defaultIncludeExts[strings.ToLower(path.Ext(rel))] ||
			defaultIncludeNames[strings.ToLower(path.Base(rel))]
	}

	return matchAny(s.cfg.Include, rel)
}

// matchAny reports whether rel matches any glob. "**" spans path segments; a
// glob is matched against both the full path and the base name, and a bare
// directory prefix (e.g. "vendor/") matches everything under it.
func matchAny(globs []string, rel string) bool {
	base := path.Base(rel)
	for _, gl := range globs {
		if gl == "" {
			continue
		}

		if strings.HasSuffix(gl, "/") && strings.HasPrefix(rel, gl) {
			return true
		}

		if globMatch(gl, rel) {
			return true
		}

		if ok, _ := path.Match(gl, base); ok {
			return true
		}
	}

	return false
}

// globMatch implements slash-aware glob matching with doublestar support. Each
// ordinary segment uses path.Match; a "**" segment consumes zero or more path
// segments.
func globMatch(pattern, name string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	nameParts := strings.Split(strings.Trim(name, "/"), "/")

	type state struct{ pattern, name int }
	memo := map[state]bool{}
	seen := map[state]bool{}

	var match func(int, int) bool
	match = func(pi, ni int) bool {
		st := state{pi, ni}
		if seen[st] {
			return memo[st]
		}
		seen[st] = true

		var ok bool
		switch {
		case pi == len(patternParts):
			ok = ni == len(nameParts)
		case patternParts[pi] == "**":
			ok = match(pi+1, ni) || (ni < len(nameParts) && match(pi, ni+1))
		case ni < len(nameParts):
			segmentOK, _ := path.Match(patternParts[pi], nameParts[ni])
			ok = segmentOK && match(pi+1, ni+1)
		}

		memo[st] = ok

		return ok
	}

	return match(0, 0)
}

// defaultIncludeExts is the source-file allowlist used when no Include globs
// are configured.
var defaultIncludeExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".java": true, ".kt": true, ".rb": true, ".rs": true, ".c": true, ".h": true,
	".cc": true, ".cpp": true, ".hpp": true, ".cs": true, ".php": true, ".swift": true,
	".scala": true, ".m": true, ".mm": true, ".sh": true, ".sql": true, ".svelte": true,
	".vue": true, ".lua": true, ".zig": true, ".ex": true, ".exs": true,
}

// defaultIncludeNames is the allowlist of extensionless or dotted-suffix source
// files (matched by base name, case-insensitively) indexed when no Include
// globs are configured. path.Ext does not classify these usefully — e.g.
// path.Ext("go.mod") is ".mod" — so they would otherwise be skipped, hiding
// dependency versions and build config from full-text search.
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

var hardSkipDirs = map[string]bool{
	".git":         true,
	"graphify-out": true,
	"krabby-docs":  true,
}

var defaultNoiseDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
}
