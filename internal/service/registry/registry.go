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

// Namespace partitioning. Every repo belongs to exactly one namespace; a repo
// with an empty stored Namespace is treated as NamespaceDefault. NamespaceAll
// is the reserved query wildcard meaning "every namespace"; it can never be
// assigned to a repo.
const (
	NamespaceDefault = "default"
	NamespaceAll     = "*"
)

// NormalizeNamespace trims and lowercases a namespace label. The empty string
// is preserved (it means "unset", equivalent to the default namespace at query
// time). NamespaceAll is preserved verbatim so query callers can pass it
// through.
func NormalizeNamespace(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == NamespaceDefault {
		// Store the default bucket as the empty string so migrated repos and
		// explicitly-defaulted repos share one representation.
		return ""
	}

	return s
}

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
	Status      string    `bw:"status,index"    json:"status"`
	LastError   string    `bw:"last_error"      json:"last_error,omitempty"`
	Namespace   string    `bw:"namespace,index" json:"namespace,omitempty"` // "" == NamespaceDefault
	Stages      Stages    `bw:"stages"          json:"stages"`
}

// NamespaceRecord is the persisted metadata for one namespace. The set of
// namespaces a repo can belong to is not limited to those with a record: a repo
// tagged into a namespace that has no record still exists as an implicit
// namespace (see Namespaces, which merges records with live repo counts). A
// record only adds a human/LLM-facing description.
type NamespaceRecord struct {
	Name        string    `bw:"name,pk"    json:"name"`
	Description string    `bw:"description" json:"description,omitempty"`
	CreatedAt   time.Time `bw:"created_at" json:"created_at,omitzero"`
	UpdatedAt   time.Time `bw:"updated_at" json:"updated_at,omitzero"`
}

// Registry stores Repo records.
type Registry struct {
	bucket   *bw.Bucket[Repo]
	nsBucket *bw.Bucket[NamespaceRecord]
}

// repoSchemaVersion must be bumped whenever the Repo struct changes shape so
// bw auto-migrates existing buckets instead of failing with a fingerprint
// mismatch at startup. v2: added per-stage generation states (Stages).
// v3: added the Namespace field (empty == default namespace).
const repoSchemaVersion = 3

// namespaceSchemaVersion mirrors repoSchemaVersion for the namespaces bucket.
const namespaceSchemaVersion = 1

// New opens the repos and namespaces buckets on the given database.
func New(db *bw.DB) (*Registry, error) {
	bucket, err := bw.RegisterBucket[Repo](db, "repos", bw.WithVersion[Repo](repoSchemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register repos bucket; %w", err)
	}

	nsBucket, err := bw.RegisterBucket[NamespaceRecord](db, "namespaces", bw.WithVersion[NamespaceRecord](namespaceSchemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register namespaces bucket; %w", err)
	}

	return &Registry{bucket: bucket, nsBucket: nsBucket}, nil
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
	Status  string
	// Namespace restricts results to one namespace. The empty string and the
	// literal "default" both select the default bucket (repos with no stored
	// namespace); NamespaceAll ("*") disables the namespace filter entirely.
	Namespace string
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

	if st := strings.TrimSpace(opts.Status); st != "" {
		q.Where = append(q.Where,
			query.NewExpressionCmp(query.OperatorEq, "status", st).Expression())
	}

	buildNamespaceFilter(q, opts.Namespace)
}

// buildNamespaceFilter appends the namespace clause. NamespaceAll ("*") adds no
// clause (all namespaces). The default bucket (empty or "default") matches
// repos whose stored namespace is empty. Any other value matches that exact
// namespace.
func buildNamespaceFilter(q *query.Query, ns string) {
	ns = strings.ToLower(strings.TrimSpace(ns))
	if ns == NamespaceAll {
		return
	}

	if ns == "" || ns == NamespaceDefault {
		q.Where = append(q.Where,
			query.NewExpressionCmp(query.OperatorEq, "namespace", "").Expression())

		return
	}

	q.Where = append(q.Where,
		query.NewExpressionCmp(query.OperatorEq, "namespace", ns).Expression())
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

// NamespaceGroup is one namespace with its repo count and optional stored
// description. It merges the live repo tally with the persisted metadata so a
// namespace shows up when it has repos, a description, or both.
type NamespaceGroup struct {
	Namespace   string `json:"namespace"`
	Count       int    `json:"count"`
	Description string `json:"description,omitempty"`
}

// Namespaces returns the distinct namespaces with their repo counts and stored
// descriptions, sorted alphabetically. Repos with no stored namespace are
// folded into the default bucket (NamespaceDefault). Namespaces that only have a
// description record (no repos yet) are still listed with count 0.
func (r *Registry) Namespaces(ctx context.Context) ([]NamespaceGroup, error) {
	counts := map[string]int{}

	q := query.New()
	q.AddField("namespace")
	if err := r.bucket.Walk(ctx, q, func(repo *Repo) error {
		ns := repo.Namespace
		if ns == "" {
			ns = NamespaceDefault
		}
		counts[ns]++

		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan repo namespaces; %w", err)
	}

	descriptions := map[string]string{}
	records, err := r.listNamespaceRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		name := rec.Name
		if name == "" {
			name = NamespaceDefault
		}
		descriptions[name] = rec.Description
		if _, ok := counts[name]; !ok {
			counts[name] = 0 // described but empty
		}
	}

	groups := make([]NamespaceGroup, 0, len(counts))
	for ns, count := range counts {
		groups = append(groups, NamespaceGroup{
			Namespace:   ns,
			Count:       count,
			Description: descriptions[ns],
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Namespace < groups[j].Namespace })

	return groups, nil
}

// listNamespaceRecords returns all stored namespace metadata records.
func (r *Registry) listNamespaceRecords(ctx context.Context) ([]*NamespaceRecord, error) {
	q, err := query.Parse("_limit=10000")
	if err != nil {
		return nil, fmt.Errorf("parse query; %w", err)
	}

	recs, err := r.nsBucket.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list namespace records; %w", err)
	}

	return recs, nil
}

// GetNamespace returns the stored record for a namespace, or nil if none exists.
// The name is normalized the same way repo namespaces are.
func (r *Registry) GetNamespace(ctx context.Context, name string) (*NamespaceRecord, error) {
	rec, err := r.nsBucket.Get(ctx, NormalizeNamespace(name))
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get namespace %s; %w", name, err)
	}

	return rec, nil
}

// UpsertNamespace creates or updates a namespace metadata record. The name is
// normalized ("default" folds to the empty stored form); NamespaceAll is
// rejected. CreatedAt is preserved across updates.
func (r *Registry) UpsertNamespace(ctx context.Context, name, description string) (*NamespaceRecord, error) {
	if strings.TrimSpace(name) == NamespaceAll {
		return nil, fmt.Errorf("namespace %q is reserved", NamespaceAll)
	}

	norm := NormalizeNamespace(name)
	now := time.Now().UTC()

	rec := &NamespaceRecord{Name: norm, Description: strings.TrimSpace(description), UpdatedAt: now, CreatedAt: now}
	if existing, err := r.GetNamespace(ctx, norm); err != nil {
		return nil, err
	} else if existing != nil {
		rec.CreatedAt = existing.CreatedAt
	}

	if err := r.nsBucket.Insert(ctx, rec); err != nil {
		return nil, fmt.Errorf("upsert namespace %s; %w", norm, err)
	}

	return rec, nil
}

// DeleteNamespace removes only the metadata record; repos tagged with the
// namespace keep their tag (the namespace becomes description-less, not empty).
// Deleting the default namespace record is a no-op-safe operation.
func (r *Registry) DeleteNamespace(ctx context.Context, name string) error {
	if err := r.nsBucket.Delete(ctx, NormalizeNamespace(name)); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete namespace %s; %w", name, err)
	}

	return nil
}

// SetNamespace assigns a repo to a namespace without disturbing its pipeline
// status. The value is normalized (trimmed, lowercased; "default" folds to the
// empty stored form). Assigning NamespaceAll is rejected because it is the
// query-wildcard, not a real namespace.
func (r *Registry) SetNamespace(ctx context.Context, id, ns string) (*Repo, error) {
	if strings.TrimSpace(ns) == NamespaceAll {
		return nil, fmt.Errorf("namespace %q is reserved", NamespaceAll)
	}

	repo, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("repo %s not found", id)
	}

	repo.Namespace = NormalizeNamespace(ns)
	if err := r.Upsert(ctx, repo); err != nil {
		return nil, err
	}

	return repo, nil
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
