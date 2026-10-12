package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/memory"
	"github.com/rytsh/krabby/internal/service/searchutil"
)

// Memory search modes mirror the docs search modes.
const (
	MemorySearchLexical  = "lexical"
	MemorySearchSemantic = "semantic"
	MemorySearchHybrid   = "hybrid"
)

// memoryDuplicateScore is the cosine similarity above which a new memory is
// treated as a restatement of an existing one in the same space.
const memoryDuplicateScore = 0.92

func (m *Manager) SetMemories(store *memory.Store) { m.memories = store }

var errMemoryUnavailable = errors.New("memory store unavailable")

// MemoryHit is one recall result.
type MemoryHit struct {
	*memory.Memory
	Score float64 `json:"score"`
}

// MemorySearch is a recall page.
type MemorySearch struct {
	Mode    string      `json:"mode"`
	Results []MemoryHit `json:"results"`
	Note    string      `json:"note,omitempty"`
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

// memoryEmbedder returns the docs embedder when semantic docs search is
// configured; memories share its model.
func (m *Manager) memoryEmbed(ctx context.Context, text string, query bool) ([]float32, string) {
	d, release := m.acquireDocs()
	defer release()
	if d == nil || d.rag == nil {
		return nil, ""
	}
	emb := d.rag.Embedder()
	if emb == nil {
		return nil, ""
	}
	var (
		vec []float32
		err error
	)
	if query {
		vec, err = emb.EmbedQuery(ctx, text)
	} else {
		var vecs [][]float32
		vecs, err = emb.Embed(ctx, []string{text})
		if err == nil && len(vecs) == 1 {
			vec = vecs[0]
		}
	}
	if err != nil || len(vec) == 0 {
		if err != nil {
			slog.Warn("embed memory", "error", err)
		}
		return nil, ""
	}
	return vec, emb.Model()
}

func memoryEmbedText(m *memory.Memory) string {
	return m.Title + "\n" + strings.Join(m.Tags, " ") + "\n" + m.Content
}

// resolveMemoryEvidence fills commit, hash and line ranges from the active
// clone. A symbol without lines resolves to its graph definition.
func (m *Manager) resolveMemoryEvidence(ctx context.Context, evidence []memory.Evidence) ([]memory.Evidence, error) {
	out := make([]memory.Evidence, 0, len(evidence))
	for _, e := range evidence {
		repo, err := m.reg.Resolve(ctx, e.Repo)
		if err != nil {
			return nil, err
		}
		if repo == nil {
			return nil, fmt.Errorf("%w: repo %s is not tracked", memory.ErrInvalid, e.Repo)
		}
		e.Repo = repo.ID
		if e.Symbol != "" && e.StartLine == 0 {
			defs, derr := m.FindDefinition(ctx, repo.ID, "", e.Symbol, 5)
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
		content, err := m.ReadRepoFile(ctx, repo.ID, e.Path, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: evidence %s:%s cannot be read: %v", memory.ErrInvalid, repo.ID, e.Path, err)
		}
		if content.Truncated && e.StartLine > 0 {
			full, ferr := m.readWholeRepoFile(ctx, repo.ID, e.Path)
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
func (m *Manager) readWholeRepoFile(ctx context.Context, repoID, path string) (string, error) {
	const limit = 4 << 20
	var b strings.Builder
	var offset int64
	for {
		part, err := m.ReadRepoFile(ctx, repoID, path, offset, 0)
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
func (m *Manager) Remember(ctx context.Context, in memory.Input) (RememberResult, error) {
	if m.memories == nil {
		return RememberResult{}, errMemoryUnavailable
	}
	in, err := in.Normalize()
	if err != nil {
		return RememberResult{}, err
	}
	if dup, err := m.memories.FindDuplicate(ctx, in.Space, memory.ContentHash(in.Title, in.Content)); err != nil {
		return RememberResult{}, err
	} else if dup != nil {
		return RememberResult{Memory: dup, Duplicate: true, Score: 1, Note: "identical memory already exists; use update_memory to change it"}, nil
	}
	evidence, err := m.resolveMemoryEvidence(ctx, in.Evidence)
	if err != nil {
		return RememberResult{}, err
	}
	in.Evidence = evidence
	vec, model := m.memoryEmbed(ctx, memoryEmbedText(&memory.Memory{Title: in.Title, Content: in.Content, Tags: in.Tags}), false)
	if vec != nil {
		candidates, err := m.memories.Candidates(ctx, memory.ListOptions{Spaces: []string{in.Space}, Status: "*"})
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
	spaces, err := m.memories.Spaces(ctx)
	if err != nil {
		return RememberResult{}, err
	}
	created, err := m.memories.Create(ctx, in, vec, model)
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

func (m *Manager) Memory(ctx context.Context, id string) (*memory.Memory, error) {
	if m.memories == nil {
		return nil, errMemoryUnavailable
	}
	return m.memories.Get(ctx, id)
}

func (m *Manager) ListMemories(ctx context.Context, opts memory.ListOptions) (memory.Page, error) {
	if m.memories == nil {
		return memory.Page{}, errMemoryUnavailable
	}
	opts, err := normalizeMemoryFilter(ctx, m, opts)
	if err != nil {
		return memory.Page{}, err
	}
	return m.memories.List(ctx, opts)
}

func (m *Manager) MemorySpaces(ctx context.Context) ([]memory.SpaceCount, error) {
	if m.memories == nil {
		return nil, errMemoryUnavailable
	}
	return m.memories.Spaces(ctx)
}

func (m *Manager) ForgetMemory(ctx context.Context, id string) error {
	if m.memories == nil {
		return errMemoryUnavailable
	}
	return m.memories.Delete(ctx, id)
}

// UpdateMemory applies a patch. Changing evidence re-pins it; changing text
// re-embeds; any edit by a caller re-activates a stale memory, because the
// caller has just reviewed it.
func (m *Manager) UpdateMemory(ctx context.Context, id string, p memory.Patch) (*memory.Memory, error) {
	if m.memories == nil {
		return nil, errMemoryUnavailable
	}
	actor, err := memory.NormalizeAuthor(p.Author)
	if err != nil {
		return nil, err
	}
	current, err := m.memories.Get(ctx, id)
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
		if evidence, err = m.resolveMemoryEvidence(ctx, in.Evidence); err != nil {
			return nil, err
		}
	} else if current.Status == memory.StatusStale || current.Status == memory.StatusOrphaned {
		// Reviewing a stale memory confirms it against today's code.
		if evidence, err = m.resolveMemoryEvidence(ctx, stripPins(keptEvidence)); err != nil {
			return nil, err
		}
	}
	textChanged := in.Title != current.Title || in.Content != current.Content || !slices.Equal(in.Tags, current.Tags)
	var vec []float32
	var model string
	if textChanged {
		vec, model = m.memoryEmbed(ctx, memoryEmbedText(&memory.Memory{Title: in.Title, Content: in.Content, Tags: in.Tags}), false)
	}
	return m.memories.Update(ctx, id, p.ExpectedVersion, func(mem *memory.Memory, emb *[]float32, embModel *string) (bool, error) {
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

func normalizeMemoryFilter(ctx context.Context, m *Manager, opts memory.ListOptions) (memory.ListOptions, error) {
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
		if repo, rerr := m.reg.Resolve(ctx, opts.Repo); rerr == nil && repo != nil {
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
func (m *Manager) RecallMemories(ctx context.Context, text, mode string, opts memory.ListOptions, limit int) (MemorySearch, error) {
	if m.memories == nil {
		return MemorySearch{}, errMemoryUnavailable
	}
	if strings.TrimSpace(text) == "" {
		return MemorySearch{}, fmt.Errorf("%w: query is empty", memory.ErrInvalid)
	}
	opts, err := normalizeMemoryFilter(ctx, m, opts)
	if err != nil {
		return MemorySearch{}, err
	}
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", MemorySearchHybrid, MemorySearchLexical, MemorySearchSemantic:
	default:
		return MemorySearch{}, fmt.Errorf("%w: mode must be lexical, semantic or hybrid", memory.ErrInvalid)
	}
	out := MemorySearch{Results: []MemoryHit{}}
	var vec []float32
	var model string
	if mode != MemorySearchLexical {
		vec, model = m.memoryEmbed(ctx, text, true)
		if vec == nil {
			if mode == MemorySearchSemantic {
				out.Note = "semantic search needs the docs embedder; fell back to lexical"
			}
			mode = MemorySearchLexical
		} else if mode == "" {
			mode = MemorySearchHybrid
		}
	}
	out.Mode = mode

	var lexical []*memory.Memory
	if mode != MemorySearchSemantic {
		var err error
		lexical, err = m.memories.SearchLexical(ctx, searchutil.LexicalQuery(text, nil), opts, limit*2)
		if err != nil {
			return MemorySearch{}, err
		}
	}
	var semantic []MemoryHit
	if mode != MemorySearchLexical {
		candidates, err := m.memories.Candidates(ctx, opts)
		if err != nil {
			return MemorySearch{}, err
		}
		missing := 0
		for _, c := range candidates {
			if c.EmbedModel != model || len(c.Embedding) != len(vec) {
				missing++
				continue
			}
			semantic = append(semantic, MemoryHit{Memory: c.Memory, Score: cosine(vec, c.Embedding)})
		}
		sort.SliceStable(semantic, func(i, j int) bool { return semantic[i].Score > semantic[j].Score })
		if len(semantic) > limit*2 {
			semantic = semantic[:limit*2]
		}
		if missing > 0 {
			m.queueMemoryEmbeddings()
		}
	}

	switch mode {
	case MemorySearchLexical:
		for i, mem := range lexical {
			out.Results = append(out.Results, MemoryHit{Memory: mem, Score: 1 / float64(i+1)})
		}
	case MemorySearchSemantic:
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
			out.Results = append(out.Results, MemoryHit{Memory: byID[id], Score: score})
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
func (m *Manager) queueMemoryEmbeddings() {
	if !m.memoryEmbedding.CompareAndSwap(false, true) {
		return
	}
	if !m.startWork() {
		m.memoryEmbedding.Store(false)
		return
	}
	go func() {
		defer m.wg.Done()
		defer m.memoryEmbedding.Store(false)
		if err := m.BackfillMemoryEmbeddings(m.baseCtx); err != nil && m.baseCtx.Err() == nil {
			slog.Warn("backfill memory embeddings", "error", err)
		}
	}()
}

func (m *Manager) BackfillMemoryEmbeddings(ctx context.Context) error {
	if m.memories == nil {
		return nil
	}
	probe, model := m.memoryEmbed(ctx, "memory", true)
	if probe == nil {
		return nil
	}
	candidates, err := m.memories.Candidates(ctx, memory.ListOptions{Status: "*"})
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
		vec, got := m.memoryEmbed(ctx, memoryEmbedText(c.Memory), false)
		if vec == nil || got != model {
			return nil
		}
		if err := m.memories.SetEmbedding(ctx, c.Memory.ID, c.Memory.Version, vec, model); err != nil {
			return err
		}
	}
	return nil
}

// RepoMemorySummary is the memory hint shown with repository overviews.
type RepoMemorySummary struct {
	Total    int `json:"total"`
	Stale    int `json:"stale,omitempty"`
	Orphaned int `json:"orphaned,omitempty"`
}

func (m *Manager) RepoMemorySummary(ctx context.Context, repoID string) (RepoMemorySummary, error) {
	var out RepoMemorySummary
	if m.memories == nil {
		return out, nil
	}
	mems, err := m.memories.ByRepo(ctx, repoID)
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
func (m *Manager) CheckRepoMemories(ctx context.Context, repoID string) error {
	if m.memories == nil {
		return nil
	}
	mems, err := m.memories.ByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	repo, err := m.reg.Get(ctx, repoID)
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
		content, err := m.readWholeRepoFile(ctx, repoID, path)
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
		_, err := m.memories.Update(ctx, mem.ID, 0, func(cur *memory.Memory, _ *[]float32, _ *string) (bool, error) {
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
func (m *Manager) OrphanRepoMemories(ctx context.Context, repoID string) error {
	if m.memories == nil {
		return nil
	}
	read := func(string) (string, bool) { return "", false }
	mems, err := m.memories.ByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	var errs []error
	for _, mem := range mems {
		_, err := m.memories.Update(ctx, mem.ID, 0, func(cur *memory.Memory, _ *[]float32, _ *string) (bool, error) {
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
