// Package memorysvc runs krabby's shared agent memory on top of the memory
// store: duplicate detection, evidence pinning against repository clones,
// lexical/semantic recall, and re-checking pins after every refresh.
//
// It used to be a slice of the manager. It reaches the rest of krabby only
// through Deps, which is the whole of what memory needs from it: resolve a
// repository, read a file from its active clone, find a symbol's definition,
// embed text, and run work in the background.
package memorysvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/memory"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/searchutil"
)

// Search modes mirror the docs search modes.
const (
	SearchLexical  = "lexical"
	SearchSemantic = "semantic"
	SearchHybrid   = "hybrid"
)

// memoryDuplicateScore is the cosine similarity above which a new memory is
// treated as a restatement of an existing one in the same space.
const memoryDuplicateScore = 0.92

// ErrUnavailable is returned when krabby runs without a memory store.
var ErrUnavailable = errors.New("memory store unavailable")

// Hit is one recall result.
type Hit struct {
	*memory.Memory
	Score float64 `json:"score"`
}

// Search is a recall page.
type Search struct {
	Mode    string `json:"mode"`
	Results []Hit  `json:"results"`
	Note    string `json:"note,omitempty"`
}

// RememberResult reports a created memory, or the existing one a duplicate
// matched instead.
type RememberResult struct {
	Memory    *memory.Memory `json:"memory"`
	Created   bool           `json:"created"`
	Duplicate bool           `json:"duplicate,omitempty"`
	Score     float64        `json:"similarity,omitempty"`
	Note      string         `json:"note,omitempty"`
	// NewSpace reports that this memory opened a space nobody used before,
	// with the spaces that already existed, so a caller that meant one of them
	// ("dev" vs "development") can move it.
	NewSpace bool     `json:"new_space,omitempty"`
	Spaces   []string `json:"existing_spaces,omitempty"`
}

// Deps is what memory needs from the rest of krabby.
type Deps struct {
	// ResolveRepo finds a tracked repository by its id or a unique suffix of
	// it; nil, nil when none matches.
	ResolveRepo func(ctx context.Context, ref string) (*registry.Repo, error)
	// Repo returns a tracked repository by exact id; nil, nil when unknown.
	Repo func(ctx context.Context, id string) (*registry.Repo, error)
	// ReadRepoFile reads part of a file from a repository's active clone.
	ReadRepoFile func(ctx context.Context, repoID, path string, offset int64, maxBytes int) (*repofs.FileContent, error)
	// FindDefinition resolves a symbol through the repository's graph.
	FindDefinition func(ctx context.Context, repoID, namespace, symbol string, limit int) (graphquery.SymbolDefs, error)
	// Embed returns the vector and model for text, or a nil vector when no
	// embedder is configured. query selects the query-side embedding.
	Embed func(ctx context.Context, text string, query bool) ([]float32, string, error)
	// Go runs fn in the background under the owner's lifecycle and reports
	// whether it was started; false means the owner is shutting down.
	Go func(fn func(ctx context.Context)) bool
}

// Service is shared memory. Its methods are safe for concurrent use. A
// Service without a store (or a nil one) answers every request with
// ErrUnavailable, and does nothing for the calls that run as a side effect of
// other work.
type Service struct {
	store *memory.Store
	deps  Deps

	// embedding guards the single background embedding backfill.
	embedding atomic.Bool
}

// New creates the service. store may be nil.
func New(store *memory.Store, deps Deps) *Service {
	return &Service{store: store, deps: deps}
}

// ready reports whether there is a store to work on. A nil *Service is valid
// and behaves like one without a store, so an owner that never configured
// memory needs no checks of its own.
func (s *Service) ready() bool { return s != nil && s.store != nil }

// memoryEmbed embeds text with the docs embedder, when one is configured;
// memories share its model. A nil vector means there is no embedder, or it
// failed, and the caller falls back to lexical behaviour.
func (s *Service) memoryEmbed(ctx context.Context, text string, query bool) ([]float32, string) {
	vec, model, err := s.deps.Embed(ctx, text, query)
	if err != nil {
		slog.Warn("embed memory", "error", err)

		return nil, ""
	}
	if len(vec) == 0 {
		return nil, ""
	}

	return vec, model
}

func memoryEmbedText(m *memory.Memory) string {
	return m.Title + "\n" + strings.Join(m.Tags, " ") + "\n" + m.Content
}

// resolveMemoryEvidence fills commit, hash and line ranges from the active
// clone. A symbol without lines resolves to its graph definition.
func (s *Service) resolveMemoryEvidence(ctx context.Context, evidence []memory.Evidence) ([]memory.Evidence, error) {
	out := make([]memory.Evidence, 0, len(evidence))
	for _, e := range evidence {
		repo, err := s.deps.ResolveRepo(ctx, e.Repo)
		if err != nil {
			return nil, err
		}
		if repo == nil {
			return nil, fmt.Errorf("%w: repo %s is not tracked", memory.ErrInvalid, e.Repo)
		}
		e.Repo = repo.ID
		if e.Symbol != "" && e.StartLine == 0 {
			defs, derr := s.deps.FindDefinition(ctx, repo.ID, "", e.Symbol, 5)
			if derr == nil {
				for _, def := range defs.Definitions {
					if def.Path == "" || (e.Path != "" && def.Path != e.Path) {
						continue
					}
					e.Path = def.Path
					if def.Line > 0 {
						e.StartLine, e.EndLine = def.Line, max(def.Line, def.EndLine)
					}
					break
				}
			}
			if e.Path == "" {
				return nil, fmt.Errorf("%w: symbol %s not found in %s; pass path", memory.ErrInvalid, e.Symbol, repo.ID)
			}
		}
		content, err := s.deps.ReadRepoFile(ctx, repo.ID, e.Path, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: evidence %s:%s cannot be read: %v", memory.ErrInvalid, repo.ID, e.Path, err)
		}
		if content.Truncated && e.StartLine > 0 {
			full, ferr := s.readWholeRepoFile(ctx, repo.ID, e.Path)
			if ferr != nil {
				return nil, ferr
			}
			content.Content = full
		}
		pinned, ok := memory.Pin(e, content.Content)
		if !ok {
			return nil, fmt.Errorf("%w: evidence lines %d-%d are outside %s", memory.ErrInvalid, e.StartLine, e.EndLine, e.Path)
		}
		pinned.Commit = repo.LastCommit
		pinned.Status = memory.EvidenceOK
		out = append(out, pinned)
	}
	return out, nil
}

// readWholeRepoFile pages through a large file. Evidence files beyond a few
// megabytes are not worth pinning by line.
func (s *Service) readWholeRepoFile(ctx context.Context, repoID, path string) (string, error) {
	const limit = 4 << 20
	var b strings.Builder
	var offset int64
	for {
		part, err := s.deps.ReadRepoFile(ctx, repoID, path, offset, 0)
		if err != nil {
			return "", err
		}
		b.WriteString(part.Content)
		offset += int64(part.Bytes)
		if !part.Truncated || part.Bytes == 0 {
			return b.String(), nil
		}
		if offset > limit {
			return "", fmt.Errorf("%w: %s is too large to pin by line; pin the file without lines", memory.ErrInvalid, path)
		}
	}
}

// Remember stores a memory unless the same space already holds the same or a
// near-identical one, in which case that memory is returned instead.
func (s *Service) Remember(ctx context.Context, in memory.Input) (RememberResult, error) {
	if !s.ready() {
		return RememberResult{}, ErrUnavailable
	}
	in, err := in.Normalize()
	if err != nil {
		return RememberResult{}, err
	}
	if dup, err := s.store.FindDuplicate(ctx, in.Space, memory.ContentHash(in.Title, in.Content)); err != nil {
		return RememberResult{}, err
	} else if dup != nil {
		return RememberResult{Memory: dup, Duplicate: true, Score: 1, Note: "identical memory already exists; use update_memory to change it"}, nil
	}
	evidence, err := s.resolveMemoryEvidence(ctx, in.Evidence)
	if err != nil {
		return RememberResult{}, err
	}
	in.Evidence = evidence
	vec, model := s.memoryEmbed(ctx, memoryEmbedText(&memory.Memory{Title: in.Title, Content: in.Content, Tags: in.Tags}), false)
	if vec != nil {
		candidates, err := s.store.Candidates(ctx, memory.ListOptions{Spaces: []string{in.Space}, Status: "*"})
		if err != nil {
			return RememberResult{}, err
		}
		var best *memory.Memory
		bestScore := 0.0
		for _, c := range candidates {
			if c.EmbedModel != model {
				continue
			}
			if score := cosine(vec, c.Embedding); score > bestScore {
				best, bestScore = c.Memory, score
			}
		}
		if best != nil && bestScore >= memoryDuplicateScore {
			return RememberResult{Memory: best, Duplicate: true, Score: bestScore, Note: "a near-identical memory already exists in this space; use update_memory to refine it"}, nil
		}
	}
	spaces, err := s.store.Spaces(ctx)
	if err != nil {
		return RememberResult{}, err
	}
	created, err := s.store.Create(ctx, in, vec, model)
	if err != nil {
		return RememberResult{}, err
	}
	out := RememberResult{Memory: created, Created: true}
	if !slices.ContainsFunc(spaces, func(s memory.SpaceCount) bool { return s.Space == in.Space }) {
		out.NewSpace = true
		out.Spaces = []string{}
		for _, s := range spaces {
			out.Spaces = append(out.Spaces, s.Space)
		}
		if len(out.Spaces) > 0 {
			out.Note = "created new space " + in.Space + "; if you meant an existing space, move it with update_memory"
		}
	}
	return out, nil
}

func (s *Service) Memory(ctx context.Context, id string) (*memory.Memory, error) {
	if !s.ready() {
		return nil, ErrUnavailable
	}
	return s.store.Get(ctx, id)
}

func (s *Service) ListMemories(ctx context.Context, opts memory.ListOptions) (memory.Page, error) {
	if !s.ready() {
		return memory.Page{}, ErrUnavailable
	}
	opts, err := normalizeMemoryFilter(ctx, s, opts)
	if err != nil {
		return memory.Page{}, err
	}
	return s.store.List(ctx, opts)
}

func (s *Service) MemorySpaces(ctx context.Context) ([]memory.SpaceCount, error) {
	if !s.ready() {
		return nil, ErrUnavailable
	}
	return s.store.Spaces(ctx)
}

func (s *Service) ForgetMemory(ctx context.Context, id string) error {
	if !s.ready() {
		return ErrUnavailable
	}
	return s.store.Delete(ctx, id)
}

// UpdateMemory applies a patch. Changing evidence re-pins it; changing text
// re-embeds; any edit by a caller re-activates a stale memory, because the
// caller has just reviewed it.
func (s *Service) UpdateMemory(ctx context.Context, id string, p memory.Patch) (*memory.Memory, error) {
	if !s.ready() {
		return nil, ErrUnavailable
	}
	actor, err := memory.NormalizeAuthor(p.Author)
	if err != nil {
		return nil, err
	}
	current, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	in := memory.Input{Space: current.Space, Kind: current.Kind, Title: current.Title, Content: current.Content, Tags: current.Tags, Evidence: current.Evidence, ExpiresAt: current.ExpiresAt, Author: current.Author}
	if p.Space != nil {
		in.Space = *p.Space
	}
	if p.Kind != nil {
		in.Kind = *p.Kind
	}
	if p.Title != nil {
		in.Title = *p.Title
	}
	if p.Content != nil {
		in.Content = *p.Content
	}
	if p.Tags != nil {
		in.Tags = *p.Tags
	}
	if p.Evidence != nil {
		in.Evidence = *p.Evidence
	}
	if p.ExpiresAt != nil {
		in.ExpiresAt = *p.ExpiresAt
	}
	keptEvidence := current.Evidence
	in, err = in.Normalize()
	if err != nil {
		return nil, err
	}
	evidence := keptEvidence
	if p.Evidence != nil {
		if evidence, err = s.resolveMemoryEvidence(ctx, in.Evidence); err != nil {
			return nil, err
		}
	} else if current.Status == memory.StatusStale || current.Status == memory.StatusOrphaned {
		// Reviewing a stale memory confirms it against today's code.
		if evidence, err = s.resolveMemoryEvidence(ctx, stripPins(keptEvidence)); err != nil {
			return nil, err
		}
	}
	textChanged := in.Title != current.Title || in.Content != current.Content || !slices.Equal(in.Tags, current.Tags)
	var vec []float32
	var model string
	if textChanged {
		vec, model = s.memoryEmbed(ctx, memoryEmbedText(&memory.Memory{Title: in.Title, Content: in.Content, Tags: in.Tags}), false)
	}
	return s.store.Update(ctx, id, p.ExpectedVersion, func(mem *memory.Memory, emb *[]float32, embModel *string) (bool, error) {
		mem.Space, mem.Kind, mem.Title, mem.Content, mem.Tags, mem.ExpiresAt = in.Space, in.Kind, in.Title, in.Content, in.Tags, in.ExpiresAt
		mem.Evidence = evidence
		mem.Status = memory.StatusFromEvidence(evidence)
		mem.UpdatedAt = time.Now().UTC()
		if textChanged {
			*emb, *embModel = vec, model
		}
		memory.AddEvent(mem, memory.Event{At: mem.UpdatedAt, Action: memory.ActionUpdated, Actor: actor, Note: p.Note})
		return true, nil
	})
}

func normalizeMemoryFilter(ctx context.Context, s *Service, opts memory.ListOptions) (memory.ListOptions, error) {
	var err error
	if opts.Spaces, err = memory.NormalizeFilterSpaces(opts.Spaces); err != nil {
		return opts, err
	}
	if opts.Kind != "" {
		if opts.Kind, err = memory.NormalizeKind(opts.Kind); err != nil {
			return opts, err
		}
	}
	switch opts.Status {
	case "", "*", memory.StatusActive, memory.StatusStale, memory.StatusOrphaned, memory.StatusExpired:
	default:
		return opts, fmt.Errorf("%w: status must be active, stale, orphaned, expired or *", memory.ErrInvalid)
	}
	if opts.Repo != "" {
		// Accept a unique suffix like the other repo-scoped tools.
		if repo, rerr := s.deps.ResolveRepo(ctx, opts.Repo); rerr == nil && repo != nil {
			opts.Repo = repo.ID
		}
	}
	return opts, nil
}

func stripPins(evidence []memory.Evidence) []memory.Evidence {
	out := slices.Clone(evidence)
	for i := range out {
		out[i].Commit, out[i].Hash, out[i].Status = "", "", ""
	}
	return out
}

// RecallMemories searches memories. Semantic and hybrid need the docs
// embedder; without it they fall back to lexical with a note.
func (s *Service) RecallMemories(ctx context.Context, text, mode string, opts memory.ListOptions, limit int) (Search, error) {
	if !s.ready() {
		return Search{}, ErrUnavailable
	}
	if strings.TrimSpace(text) == "" {
		return Search{}, fmt.Errorf("%w: query is empty", memory.ErrInvalid)
	}
	opts, err := normalizeMemoryFilter(ctx, s, opts)
	if err != nil {
		return Search{}, err
	}
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", SearchHybrid, SearchLexical, SearchSemantic:
	default:
		return Search{}, fmt.Errorf("%w: mode must be lexical, semantic or hybrid", memory.ErrInvalid)
	}
	out := Search{Results: []Hit{}}
	var vec []float32
	var model string
	if mode != SearchLexical {
		vec, model = s.memoryEmbed(ctx, text, true)
		if vec == nil {
			if mode == SearchSemantic {
				out.Note = "semantic search needs the docs embedder; fell back to lexical"
			}
			mode = SearchLexical
		} else if mode == "" {
			mode = SearchHybrid
		}
	}
	out.Mode = mode

	var lexical []*memory.Memory
	if mode != SearchSemantic {
		var err error
		lexical, err = s.store.SearchLexical(ctx, searchutil.LexicalQuery(text, nil), opts, limit*2)
		if err != nil {
			return Search{}, err
		}
	}
	var semantic []Hit
	if mode != SearchLexical {
		candidates, err := s.store.Candidates(ctx, opts)
		if err != nil {
			return Search{}, err
		}
		missing := 0
		for _, c := range candidates {
			if c.EmbedModel != model || len(c.Embedding) != len(vec) {
				missing++
				continue
			}
			semantic = append(semantic, Hit{Memory: c.Memory, Score: cosine(vec, c.Embedding)})
		}
		sort.SliceStable(semantic, func(i, j int) bool { return semantic[i].Score > semantic[j].Score })
		if len(semantic) > limit*2 {
			semantic = semantic[:limit*2]
		}
		if missing > 0 {
			s.queueMemoryEmbeddings()
		}
	}

	switch mode {
	case SearchLexical:
		for i, mem := range lexical {
			out.Results = append(out.Results, Hit{Memory: mem, Score: 1 / float64(i+1)})
		}
	case SearchSemantic:
		out.Results = append(out.Results, semantic...)
	default:
		// Reciprocal rank fusion over both arms.
		const k = 20.0
		scores := map[string]float64{}
		byID := map[string]*memory.Memory{}
		for i, mem := range lexical {
			scores[mem.ID] += 1 / (k + float64(i+1))
			byID[mem.ID] = mem
		}
		for i, hit := range semantic {
			scores[hit.ID] += 1 / (k + float64(i+1))
			byID[hit.ID] = hit.Memory
		}
		for id, score := range scores {
			out.Results = append(out.Results, Hit{Memory: byID[id], Score: score})
		}
		sort.SliceStable(out.Results, func(i, j int) bool {
			if out.Results[i].Score != out.Results[j].Score {
				return out.Results[i].Score > out.Results[j].Score
			}
			return out.Results[i].UpdatedAt.After(out.Results[j].UpdatedAt)
		})
	}
	if len(out.Results) > limit {
		out.Results = out.Results[:limit]
	}
	return out, nil
}

// queueMemoryEmbeddings backfills embeddings for memories written before an
// embedder was configured or under another model. One pass runs at a time.
func (s *Service) queueMemoryEmbeddings() {
	if !s.embedding.CompareAndSwap(false, true) {
		return
	}
	started := s.deps.Go(func(ctx context.Context) {
		defer s.embedding.Store(false)
		if err := s.BackfillMemoryEmbeddings(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("backfill memory embeddings", "error", err)
		}
	})
	if !started {
		s.embedding.Store(false)
	}
}

func (s *Service) BackfillMemoryEmbeddings(ctx context.Context) error {
	if !s.ready() {
		return nil
	}
	probe, model := s.memoryEmbed(ctx, "memory", true)
	if probe == nil {
		return nil
	}
	candidates, err := s.store.Candidates(ctx, memory.ListOptions{Status: "*"})
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.EmbedModel == model && len(c.Embedding) == len(probe) {
			continue
		}
		vec, got := s.memoryEmbed(ctx, memoryEmbedText(c.Memory), false)
		if vec == nil || got != model {
			return nil
		}
		if err := s.store.SetEmbedding(ctx, c.Memory.ID, c.Memory.Version, vec, model); err != nil {
			return err
		}
	}
	return nil
}

// RepoSummary is the memory hint shown with repository overviews.
type RepoSummary struct {
	Total    int `json:"total"`
	Stale    int `json:"stale,omitempty"`
	Orphaned int `json:"orphaned,omitempty"`
}

func (s *Service) Summarize(ctx context.Context, repoID string) (RepoSummary, error) {
	var out RepoSummary
	if !s.ready() {
		return out, nil
	}
	mems, err := s.store.ByRepo(ctx, repoID)
	if err != nil {
		return out, err
	}
	for _, mem := range mems {
		switch mem.Status {
		case memory.StatusExpired:
			continue
		case memory.StatusStale:
			out.Stale++
		case memory.StatusOrphaned:
			out.Orphaned++
		}
		out.Total++
	}
	return out, nil
}

// CheckRepoMemories re-verifies every memory pinned in repoID against its
// active clone. It runs after each successful refresh.
func (s *Service) CheckRepoMemories(ctx context.Context, repoID string) error {
	if !s.ready() {
		return nil
	}
	mems, err := s.store.ByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	repo, err := s.deps.Repo(ctx, repoID)
	if err != nil {
		return err
	}
	commit := ""
	if repo != nil {
		commit = repo.LastCommit
	}
	files := map[string]*string{}
	read := func(path string) (string, bool) {
		if cached, ok := files[path]; ok {
			if cached == nil {
				return "", false
			}
			return *cached, true
		}
		if repo == nil {
			files[path] = nil
			return "", false
		}
		content, err := s.readWholeRepoFile(ctx, repoID, path)
		if err != nil {
			files[path] = nil
			return "", false
		}
		files[path] = &content
		return content, true
	}
	var errs []error
	for _, mem := range mems {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := s.store.Update(ctx, mem.ID, 0, func(cur *memory.Memory, _ *[]float32, _ *string) (bool, error) {
			return applyEvidenceCheck(cur, repoID, commit, read), nil
		})
		if err != nil && !errors.Is(err, memory.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// applyEvidenceCheck updates one memory's evidence for repoID and reports
// whether anything changed. Status transitions are recorded in history.
func applyEvidenceCheck(cur *memory.Memory, repoID, commit string, read func(string) (string, bool)) bool {
	changed, moved := false, false
	for i, e := range cur.Evidence {
		if e.Repo != repoID {
			continue
		}
		content, exists := read(e.Path)
		checked := memory.Check(e, content, exists)
		if checked.Status == memory.EvidenceMoved {
			moved = true
			checked.Status = memory.EvidenceOK
		}
		if checked.Status == memory.EvidenceOK && exists {
			checked.Commit = commit
		}
		if checked != e {
			cur.Evidence[i] = checked
			changed = true
		}
	}
	if !changed {
		return false
	}
	prev := cur.Status
	cur.Status = memory.StatusFromEvidence(cur.Evidence)
	now := time.Now().UTC()
	switch {
	case cur.Status == prev && moved:
		memory.AddEvent(cur, memory.Event{At: now, Action: memory.ActionMoved, Actor: "krabby", Note: "pinned lines moved in " + repoID + " @ " + shortSHA(commit)})
	case cur.Status == prev:
	case cur.Status == memory.StatusStale:
		memory.AddEvent(cur, memory.Event{At: now, Action: memory.ActionStale, Actor: "krabby", Note: "pinned code changed in " + repoID + " @ " + shortSHA(commit)})
	case cur.Status == memory.StatusOrphaned:
		memory.AddEvent(cur, memory.Event{At: now, Action: memory.ActionOrphaned, Actor: "krabby", Note: "pinned file or repository is gone: " + repoID})
	default:
		memory.AddEvent(cur, memory.Event{At: now, Action: memory.ActionRestored, Actor: "krabby", Note: "pinned code matches again in " + repoID + " @ " + shortSHA(commit)})
	}
	return true
}

// OrphanRepoMemories marks memories pinned to a removed repository.
func (s *Service) OrphanRepoMemories(ctx context.Context, repoID string) error {
	if !s.ready() {
		return nil
	}
	read := func(string) (string, bool) { return "", false }
	mems, err := s.store.ByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	var errs []error
	for _, mem := range mems {
		_, err := s.store.Update(ctx, mem.ID, 0, func(cur *memory.Memory, _ *[]float32, _ *string) (bool, error) {
			return applyEvidenceCheck(cur, repoID, "", read), nil
		})
		if err != nil && !errors.Is(err, memory.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// shortSHA abbreviates a commit for a history note.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}

	return sha
}
