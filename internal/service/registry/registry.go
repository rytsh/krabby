// Package registry persists the set of tracked repositories in a bw bucket.
package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"
)

// Repo status values.
const (
	StatusPending  = "pending"
	StatusCloning  = "cloning"
	StatusBuilding = "building"
	StatusReady    = "ready"
	StatusError    = "error"
)

// Generation stage names. Each stage produces one artifact and can be run
// selectively via Manager.Generate.
const (
	StageGraph     = "graph"
	StageDocs      = "docs"
	StageDocsIndex = "docs_index"
	StageCodeIndex = "code_index"
)

// Stage status values. An empty status means the stage never ran.
const (
	StageRunning = "running"
	StageOK      = "ok"
	StageError   = "error"
)

// ValidStage reports whether name is a known generation stage.
func ValidStage(name string) bool {
	switch name {
	case StageGraph, StageDocs, StageDocsIndex, StageCodeIndex:
		return true
	}

	return false
}

// StageState records the last outcome of one generation stage.
type StageState struct {
	Status     string    `bw:"status"      json:"status,omitempty"` // "", running, ok, error
	Error      string    `bw:"error"       json:"error,omitempty"`
	Commit     string    `bw:"commit"      json:"commit,omitempty"` // commit the stage last ran against
	FinishedAt time.Time `bw:"finished_at" json:"finished_at,omitzero"`
}

// Stages groups the per-artifact generation states of a repo.
type Stages struct {
	Graph     StageState `bw:"graph"      json:"graph"`
	Docs      StageState `bw:"docs"       json:"docs"`
	DocsIndex StageState `bw:"docs_index" json:"docs_index"`
	CodeIndex StageState `bw:"code_index" json:"code_index"`
}

// Get returns a mutable pointer to the named stage, or nil for unknown names.
func (s *Stages) Get(name string) *StageState {
	switch name {
	case StageGraph:
		return &s.Graph
	case StageDocs:
		return &s.Docs
	case StageDocsIndex:
		return &s.DocsIndex
	case StageCodeIndex:
		return &s.CodeIndex
	}

	return nil
}

// Repo is a tracked repository record.
type Repo struct {
	ID          string    `bw:"id,pk"        json:"id"` // full path: host/group/.../name
	URL         string    `bw:"url"          json:"url"`
	Branch      string    `bw:"branch"       json:"branch,omitempty"`
	Path        string    `bw:"path"         json:"path"`
	LastCommit  string    `bw:"last_commit"  json:"last_commit,omitempty"`
	LastSyncAt  time.Time `bw:"last_sync"    json:"last_sync_at,omitzero"`
	LastBuildAt time.Time `bw:"last_build"   json:"last_build_at,omitzero"`
	Status      string    `bw:"status,index" json:"status"`
	LastError   string    `bw:"last_error"   json:"last_error,omitempty"`
	Stages      Stages    `bw:"stages"       json:"stages"`
}

// Registry stores Repo records.
type Registry struct {
	bucket *bw.Bucket[Repo]
}

// repoSchemaVersion must be bumped whenever the Repo struct changes shape so
// bw auto-migrates existing buckets instead of failing with a fingerprint
// mismatch at startup. v2: added per-stage generation states (Stages).
const repoSchemaVersion = 2

// New opens the repos bucket on the given database.
func New(db *bw.DB) (*Registry, error) {
	bucket, err := bw.RegisterBucket[Repo](db, "repos", bw.WithVersion[Repo](repoSchemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register repos bucket; %w", err)
	}

	return &Registry{bucket: bucket}, nil
}

// Get returns a repo by id, or nil if it does not exist.
func (r *Registry) Get(ctx context.Context, id string) (*Repo, error) {
	repo, err := r.bucket.Get(ctx, id)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get repo %s; %w", id, err)
	}

	return repo, nil
}

// List returns all tracked repos.
func (r *Registry) List(ctx context.Context) ([]*Repo, error) {
	q, err := query.Parse("_limit=10000")
	if err != nil {
		return nil, fmt.Errorf("parse query; %w", err)
	}

	repos, err := r.bucket.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list repos; %w", err)
	}

	if repos == nil {
		repos = []*Repo{}
	}

	return repos, nil
}

// ListOptions controls server-side pagination, search and sorting for
// ListPaged. Page is 1-based; a Page or PerPage <= 0 falls back to sane
// defaults. Search matches the repo id (host/group/.../name)
// case-insensitively. Owner, when set, restricts the result to the direct
// children of one directory prefix (everything before the repo name).
type ListOptions struct {
	Page    int
	PerPage int
	Search  string
	Owner   string
}

const (
	defaultPerPage = 20
	maxPerPage     = 200
)

// buildFilter assembles the shared WHERE clause for search/owner filtering so
// ListPaged and Count stay in sync.
//
// Filtering uses the bw engine's case-insensitive LIKE. bw's LIKE does not
// support wildcard escaping, so a literal '%' or '_' in user input is treated
// as a wildcard ('_' matches any single char). This is a harmless usability
// quirk for repo-id search, never a correctness or safety issue.
func buildFilter(q *query.Query, opts ListOptions) {
	if owner := strings.TrimSpace(opts.Owner); owner != "" {
		// Direct children of the owner directory only: "owner/<name>" matches,
		// "owner/<sub>/<name>" does not (that repo belongs to a deeper group).
		q.Where = append(q.Where,
			query.NewExpressionCmp(query.OperatorILike, "id", owner+"/%").Expression(),
			query.NewExpressionCmp(query.OperatorNILike, "id", owner+"/%/%").Expression())
	}

	if s := strings.TrimSpace(opts.Search); s != "" {
		q.Where = append(q.Where,
			query.NewExpressionCmp(query.OperatorILike, "id", "%"+s+"%").Expression())
	}
}

// PageParams returns the page and perPage actually applied by ListPaged for the
// given options, clamping to the same defaults and maximum. Callers use it to
// echo the effective pagination back to clients without duplicating the limits.
func PageParams(opts ListOptions) (page, perPage int) {
	perPage = opts.PerPage
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}

	page = opts.Page
	if page <= 0 {
		page = 1
	}

	return page, perPage
}

// ListPaged returns one page of repos ordered by id, plus the total number of
// records matching the same filter (ignoring pagination).
func (r *Registry) ListPaged(ctx context.Context, opts ListOptions) (repos []*Repo, total int, err error) {
	page, perPage := PageParams(opts)

	total, err = r.Count(ctx, opts)
	if err != nil {
		return nil, 0, err
	}

	q := query.New()
	buildFilter(q, opts)
	q.Sort = []query.ExpressionSort{{Field: "id"}}
	q.SetOffset(uint64((page - 1) * perPage))
	q.SetLimit(uint64(perPage))

	repos, err = r.bucket.Find(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("list repos; %w", err)
	}

	if repos == nil {
		repos = []*Repo{}
	}

	return repos, total, nil
}

// Count returns the number of repos matching the search/owner filter.
func (r *Registry) Count(ctx context.Context, opts ListOptions) (int, error) {
	q := query.New()
	buildFilter(q, opts)

	n, err := r.bucket.Count(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("count repos; %w", err)
	}

	return int(n), nil
}

// OwnerGroup is one directory prefix (everything before the repo name) and
// how many repos it holds directly.
type OwnerGroup struct {
	Owner string `json:"owner"`
	Count int    `json:"count"`
}

// Owners returns the distinct directory prefixes (the id up to the last "/")
// with their direct repo counts, sorted alphabetically. Repos without a "/"
// are grouped under the empty owner "". Only the ids are scanned, so this
// stays cheap even with many repositories.
func (r *Registry) Owners(ctx context.Context) ([]OwnerGroup, error) {
	counts := map[string]int{}

	q := query.New()
	q.AddField("id")
	if err := r.bucket.Walk(ctx, q, func(repo *Repo) error {
		owner := ""
		if idx := strings.LastIndexByte(repo.ID, '/'); idx > 0 {
			owner = repo.ID[:idx]
		}
		counts[owner]++

		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan repo owners; %w", err)
	}

	groups := make([]OwnerGroup, 0, len(counts))
	for owner, count := range counts {
		groups = append(groups, OwnerGroup{Owner: owner, Count: count})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Owner < groups[j].Owner })

	return groups, nil
}

// Resolve returns the repo identified by ref. An exact id match wins; when
// none exists, ref is treated as a path suffix (e.g. the legacy "owner/name"
// form, a webhook full_name, or a hostless path) and matched against the
// tracked ids segment-wise. A unique suffix match resolves; an ambiguous one
// returns an error listing the candidates so callers can be precise.
func (r *Registry) Resolve(ctx context.Context, ref string) (*Repo, error) {
	ref = strings.Trim(strings.TrimSpace(ref), "/")
	if ref == "" {
		return nil, nil
	}

	repo, err := r.Get(ctx, ref)
	if err != nil || repo != nil {
		return repo, err
	}

	suffix := "/" + ref

	var matches []string

	q := query.New()
	q.AddField("id")
	if err := r.bucket.Walk(ctx, q, func(rec *Repo) error {
		if strings.HasSuffix(rec.ID, suffix) {
			matches = append(matches, rec.ID)
		}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan repo ids; %w", err)
	}

	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return r.Get(ctx, matches[0])
	default:
		sort.Strings(matches)

		return nil, fmt.Errorf("ambiguous repo %q: matches %s", ref, strings.Join(matches, ", "))
	}
}

// Upsert inserts or replaces a repo record.
func (r *Registry) Upsert(ctx context.Context, repo *Repo) error {
	if err := r.bucket.Insert(ctx, repo); err != nil {
		return fmt.Errorf("upsert repo %s; %w", repo.ID, err)
	}

	return nil
}

// Delete removes a repo record.
func (r *Registry) Delete(ctx context.Context, id string) error {
	if err := r.bucket.Delete(ctx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete repo %s; %w", id, err)
	}

	return nil
}
