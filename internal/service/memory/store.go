package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"
)

const bucketName = "memories"

// record is the stored shape: the public Memory plus the embedding, which is
// never serialised to clients. Memories are few compared with code chunks, so
// semantic ranking is an exact scan over these embeddings rather than a
// separate vector index that would have to follow the docs embedder's store.
type record struct {
	Memory
	Embedding  []float32 `bw:"embedding"`
	EmbedModel string    `bw:"embed_model"`
}

type Store struct {
	bucket *bw.Bucket[record]
	mu     sync.Mutex // serializes read-modify-write cycles
}

func New(db *bw.DB) (*Store, error) {
	bucket, err := bw.RegisterBucket[record](db, bucketName, bw.WithVersion[record](1))
	if err != nil {
		return nil, fmt.Errorf("register memory bucket; %w", err)
	}
	return &Store{bucket: bucket}, nil
}

// ListOptions filters a listing. Spaces lists the spaces to include; empty or
// containing "*" means every space. Status empty hides expired memories, "*"
// includes everything.
type ListOptions struct {
	Spaces                         []string
	Kind, Status, Repo, Tag, Query string
	Page, PerPage                  int
}

type Page struct {
	Items   []*Memory `json:"items"`
	Total   int       `json:"total"`
	Page    int       `json:"page"`
	PerPage int       `json:"per_page"`
}

type SpaceCount struct {
	Space  string         `json:"space"`
	Count  int            `json:"count"`
	Status map[string]int `json:"status"`
}

// Candidate is a stored memory with its embedding, used for semantic ranking.
type Candidate struct {
	Memory     *Memory
	Embedding  []float32
	EmbedModel string
}

func (s *Store) get(ctx context.Context, id string) (*record, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	rec, err := s.bucket.Get(ctx, id)
	if errors.Is(err, bw.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	normalizeRecord(rec)
	return rec, nil
}

func normalizeRecord(rec *record) {
	if rec.Tags == nil {
		rec.Tags = []string{}
	}
	if rec.Evidence == nil {
		rec.Evidence = []Evidence{}
	}
	if rec.History == nil {
		rec.History = []Event{}
	}
}

func (s *Store) Get(ctx context.Context, id string) (*Memory, error) {
	rec, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	return view(rec, time.Now()), nil
}

func view(rec *record, now time.Time) *Memory {
	m := rec.Memory
	m.Tags = slices.Clone(rec.Tags)
	m.Evidence = slices.Clone(rec.Evidence)
	m.History = slices.Clone(rec.History)
	m.Status = rec.EffectiveStatus(now)
	return &m
}

// FindDuplicate returns a memory in space with exactly the same normalized
// title and content.
func (s *Store) FindDuplicate(ctx context.Context, space, hash string) (*Memory, error) {
	q := query.New()
	q.Where = append(q.Where,
		query.NewExpressionCmp(query.OperatorEq, "content_hash", hash).Expression(),
		query.NewExpressionCmp(query.OperatorEq, "space", space).Expression(),
	)
	q.SetLimit(1)
	recs, err := s.bucket.Find(ctx, q)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	normalizeRecord(recs[0])
	return view(recs[0], time.Now()), nil
}

// Create stores a normalized, evidence-resolved memory.
func (s *Store) Create(ctx context.Context, in Input, embedding []float32, model string) (*Memory, error) {
	now := time.Now().UTC()
	rec := &record{Memory: Memory{
		ID: NewID(), Space: in.Space, Kind: in.Kind, Title: in.Title, Content: in.Content,
		Tags: in.Tags, Evidence: in.Evidence, Author: in.Author, Version: 1,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: in.ExpiresAt,
	}, Embedding: embedding, EmbedModel: model}
	rec.Status = StatusFromEvidence(rec.Evidence)
	AddEvent(&rec.Memory, Event{At: now, Action: ActionCreated, Actor: rec.Author})
	finalize(rec)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.bucket.Insert(ctx, rec); err != nil {
		return nil, err
	}
	return view(rec, now), nil
}

func finalize(rec *record) {
	normalizeRecord(rec)
	rec.SearchText = searchText(&rec.Memory)
	rec.UpdatedUnix = rec.UpdatedAt.UnixNano()
	rec.ContentHash = ContentHash(rec.Title, rec.Content)
}

// Update applies fn to the stored memory under the store lock. fn returns
// whether the record changed.
func (s *Store) Update(ctx context.Context, id string, expectedVersion uint64, fn func(*Memory, *[]float32, *string) (bool, error)) (*Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if expectedVersion != 0 && rec.Version != expectedVersion {
		return nil, ErrConflict
	}
	changed, err := fn(&rec.Memory, &rec.Embedding, &rec.EmbedModel)
	if err != nil {
		return nil, err
	}
	if !changed {
		return view(rec, time.Now()), nil
	}
	rec.Version++
	finalize(rec)
	if err := s.bucket.Insert(ctx, rec); err != nil {
		return nil, err
	}
	return view(rec, time.Now()), nil
}

// SetEmbedding stores an embedding without touching the version or history:
// it is derived data.
func (s *Store) SetEmbedding(ctx context.Context, id string, version uint64, embedding []float32, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if rec.Version != version {
		return nil // edited meanwhile; the editor embedded the new text
	}
	rec.Embedding, rec.EmbedModel = embedding, model
	return s.bucket.Insert(ctx, rec)
}

func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.get(ctx, id); err != nil {
		return err
	}
	return s.bucket.Delete(ctx, id)
}

func filterQuery(spaces []string, kind string) *query.Query {
	q := query.New()
	switch {
	case len(spaces) == 0 || slices.Contains(spaces, "*"):
	case len(spaces) == 1:
		q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "space", spaces[0]).Expression())
	default:
		q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorIn, "space", spaces).Expression())
	}
	if kind != "" {
		q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "kind", kind).Expression())
	}
	return q
}

// matches applies the filters bw cannot index: tags, evidence repositories
// (a memory may cite several) and the time-derived expired status.
func matches(m *Memory, opts ListOptions, tag string, now time.Time) bool {
	status := opts.Status
	if tag != "" && !slices.Contains(m.Tags, tag) {
		return false
	}
	if opts.Repo != "" && !slices.ContainsFunc(m.Evidence, func(e Evidence) bool { return e.Repo == opts.Repo }) {
		return false
	}
	effective := m.EffectiveStatus(now)
	if status == "" {
		return effective != StatusExpired
	}
	return status == "*" || status == effective
}

// List returns one page ordered by most recently updated.
func (s *Store) List(ctx context.Context, opts ListOptions) (Page, error) {
	page, perPage := max(1, opts.Page), opts.PerPage
	if perPage <= 0 {
		perPage = 20
	}
	perPage = min(perPage, 100)
	result := Page{Items: []*Memory{}, Page: page, PerPage: perPage}
	now := time.Now()
	text := strings.ToLower(strings.TrimSpace(opts.Query))
	tag := strings.ToLower(strings.TrimSpace(opts.Tag))
	var all []*Memory
	err := s.bucket.Walk(ctx, filterQuery(opts.Spaces, opts.Kind), func(rec *record) error {
		normalizeRecord(rec)
		if !matches(&rec.Memory, opts, tag, now) {
			return nil
		}
		if text != "" && !strings.Contains(strings.ToLower(rec.Title+"\n"+rec.Content+"\n"+rec.SearchText), text) {
			return nil
		}
		all = append(all, view(rec, now))
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
	result.Total = len(all)
	if page > (result.Total+perPage-1)/perPage {
		return result, nil
	}
	start := (page - 1) * perPage
	result.Items = all[start:min(start+perPage, len(all))]
	return result, nil
}

// SearchLexical ranks memories with BM25 over title, content, tags and paths.
func (s *Store) SearchLexical(ctx context.Context, text string, opts ListOptions, limit int) ([]*Memory, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: query is empty", ErrInvalid)
	}
	now := time.Now()
	tag := strings.ToLower(strings.TrimSpace(opts.Tag))
	out := []*Memory{}
	q := filterQuery(opts.Spaces, opts.Kind)
	var filter any
	if len(q.Where) > 0 {
		filter = q
	}
	_, err := s.bucket.SearchWalk(ctx, text, bw.SearchOptions{Filter: filter}, func(hit bw.SearchResult[record]) (bool, error) {
		if hit.Record == nil {
			return true, nil
		}
		normalizeRecord(hit.Record)
		if matches(&hit.Record.Memory, opts, tag, now) {
			out = append(out, view(hit.Record, now))
		}
		return len(out) < limit, nil
	})
	return out, err
}

// Candidates returns every memory matching opts with its embedding.
func (s *Store) Candidates(ctx context.Context, opts ListOptions) ([]Candidate, error) {
	now := time.Now()
	tag := strings.ToLower(strings.TrimSpace(opts.Tag))
	var out []Candidate
	err := s.bucket.Walk(ctx, filterQuery(opts.Spaces, opts.Kind), func(rec *record) error {
		normalizeRecord(rec)
		if matches(&rec.Memory, opts, tag, now) {
			out = append(out, Candidate{Memory: view(rec, now), Embedding: rec.Embedding, EmbedModel: rec.EmbedModel})
		}
		return nil
	})
	return out, err
}

// ByRepo returns every memory whose evidence references repo, including
// memories whose first evidence is in another repository.
func (s *Store) ByRepo(ctx context.Context, repo string) ([]*Memory, error) {
	var out []*Memory
	now := time.Now()
	err := s.bucket.Walk(ctx, query.New(), func(rec *record) error {
		normalizeRecord(rec)
		if slices.ContainsFunc(rec.Evidence, func(e Evidence) bool { return e.Repo == repo }) {
			out = append(out, view(rec, now))
		}
		return nil
	})
	return out, err
}

func (s *Store) Spaces(ctx context.Context) ([]SpaceCount, error) {
	counts := map[string]*SpaceCount{}
	now := time.Now()
	err := s.bucket.Walk(ctx, query.New(), func(rec *record) error {
		c := counts[rec.Space]
		if c == nil {
			c = &SpaceCount{Space: rec.Space, Status: map[string]int{}}
			counts[rec.Space] = c
		}
		c.Count++
		c.Status[rec.EffectiveStatus(now)]++
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]SpaceCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Space < out[j].Space })
	return out, nil
}
