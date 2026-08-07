// Package apicatalog tracks machine-readable API descriptions — OpenAPI/Swagger
// documents and gRPC servers — as a browsable catalog of groups, services and
// operations.
//
// It is deliberately not a websource fetcher. A web source's unit is a markdown
// blob keyed by a slug, which is the right shape for prose and the wrong shape
// for an API: answering "what does this endpoint take" needs the method, the
// path, the parameters and the request schema as data, not as text. So the
// catalog stores operations structurally and *also* projects each one to
// markdown, which rides the shared docs RAG index under the "api:<service>"
// scope key. The structure serves precise navigation; the projection serves
// "which endpoint creates an invoice".
//
// The three levels mirror concepts that already exist elsewhere in krabby:
//
//	Group     ~ registry namespace  — a named bucket with a description, the
//	                                  thing a model reads to pick a target
//	Service   ~ websource.Collection — one spec document or one gRPC server,
//	                                  with a provider, a schedule and a status
//	Operation ~ websource.Page       — one endpoint, the unit that is listed,
//	                                  fetched and indexed
//
// Providers live in per-kind subpackages (apicatalog/openapi, apicatalog/grpc)
// and implement Provider; a new kind adds a subpackage and registers itself in
// the manager wiring.
//
// # Overrides
//
// A published spec is frequently wrong for the reader's environment: it names
// a public base URL when the caller needs the internal one, its descriptions
// are thin, or a schema does not match what the service actually deployed.
// Rather than force the spec to be fixed at the source, a service carries three
// layers of override, applied in this order:
//
//  1. SpecPatch — an RFC 7386 JSON Merge Patch applied to the raw document
//     before it is parsed. The general escape hatch: it can reach any field,
//     including schemas.
//  2. BaseURL — replaces whatever servers the parsed document declares.
//  3. Operations — per-operation summary/description/tags/hidden, applied as
//     each operation is walked.
//
// The layers are ordered from most general to most specific so a targeted fix
// is never undone by a broad one.
package apicatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// Service kinds. Each kind has a provider implementation in its own subpackage.
const (
	KindOpenAPI = "openapi" // OpenAPI 2.0/3.x document fetched over HTTP
	KindGRPC    = "grpc"    // gRPC server enumerated via server reflection
)

// Service status values.
const (
	StatusPending  = "pending"
	StatusFetching = "fetching"
	StatusReady    = "ready"
	StatusError    = "error"
)

// GroupDefault is where services with no explicit group are listed. It is a
// display name only: the stored value is the empty string, exactly as repo
// namespaces fold "default" to "".
const GroupDefault = "default"

// MethodGRPC is the pseudo-method recorded for a gRPC unary/streaming call, so
// one Operation shape covers both transports.
const MethodGRPC = "GRPC"

// ScopePrefix namespaces API-catalog keys in the shared docs index.
//
// The convention is owned by the store, which has to answer questions about
// it; this is a re-export so callers reading catalog code do not have to reach
// across packages for the spelling.
const ScopePrefix = vectorstore.APIScopePrefix

// ScopeKey returns the vector-store key of a service ("api:<name>").
func ScopeKey(name string) string { return ScopePrefix + name }

// ServiceName returns the service name of a scope key, or "" when the key is
// not an API-catalog key.
func ServiceName(scopeKey string) string {
	if !strings.HasPrefix(scopeKey, ScopePrefix) {
		return ""
	}

	return strings.TrimPrefix(scopeKey, ScopePrefix)
}

// Group is a named bucket of services with a human-written description.
//
// The description is the point of the type. A model choosing where to look
// reads group descriptions first, so "billing — invoicing, payments and dunning
// APIs" is what turns a list of opaque service names into a decision. Groups
// are created implicitly: naming one on a service is enough, and the record
// here only adds the description.
type Group struct {
	Name        string    `bw:"name,pk"      json:"name"`
	Description string    `bw:"description"  json:"description,omitempty"`
	UpdatedAt   time.Time `bw:"updated_at"   json:"updated_at,omitzero"`
	CreatedAt   time.Time `bw:"created_at"   json:"created_at,omitzero"`
}

// Service is one API description: an OpenAPI document or a gRPC server.
type Service struct {
	// Name is the user-chosen identifier (e.g. "billing"). It is used in file
	// paths and as the search scope key, so it is restricted to
	// [a-z0-9][a-z0-9._-]*.
	Name string `bw:"name,pk" json:"name"`
	// Group is the bucket this service belongs to; empty means GroupDefault.
	Group string `bw:"group,index" json:"group"`
	// Kind selects the provider: KindOpenAPI or KindGRPC. Immutable once set,
	// because changing it would reinterpret every stored operation.
	Kind string `bw:"kind,index" json:"kind"`

	// Title and Version come from the spec (info.title / info.version) and are
	// refreshed on every sync.
	Title   string `bw:"title"   json:"title,omitempty"`
	Version string `bw:"version" json:"version,omitempty"`
	// SpecSummary is the spec's own description, trimmed. Kept separate from
	// Description so a refresh can update it without clobbering what a human
	// wrote.
	SpecSummary string `bw:"spec_summary" json:"spec_summary,omitempty"`
	// Description is the human-written override surfaced to MCP clients. When
	// set it wins over SpecSummary; when empty SpecSummary is shown.
	Description string `bw:"description" json:"description,omitempty"`

	// BaseURL overrides the servers the document declares. Published specs
	// routinely name a public or example host while the reader needs the
	// internal deployment, and that discrepancy makes every generated request
	// recipe wrong. Empty means "use what the spec says".
	BaseURL string `bw:"base_url" json:"base_url,omitempty"`
	// ResolvedBaseURL is what the last sync actually settled on: BaseURL when
	// set, otherwise the first server in the document. Stored so listings and
	// request recipes do not have to re-read the spec.
	ResolvedBaseURL string `bw:"resolved_base_url" json:"resolved_base_url,omitempty"`

	// SpecPatch is an RFC 7386 JSON Merge Patch applied to the raw document
	// before parsing. It is the general override: anything reachable in the
	// document, schemas included, can be corrected here. Null values delete.
	SpecPatch json.RawMessage `bw:"spec_patch" json:"spec_patch,omitempty"`
	// Operations holds per-operation overrides keyed by operation id (the
	// spec's operationId, or "METHOD /path" when it has none). Applied last,
	// so a targeted fix survives a broad SpecPatch.
	Operations map[string]OperationOverride `bw:"operations" json:"operations,omitempty"`

	// RefreshInterval is how often the scheduler re-syncs the service.
	// 0 disables automatic refresh (manual only). It is the fallback when
	// Specs is empty.
	RefreshInterval time.Duration `bw:"refresh_interval" json:"refresh_interval"`
	// Specs are cron schedules (hardloop syntax) on which the scheduler
	// re-syncs. When non-empty they are authoritative and RefreshInterval is
	// ignored.
	Specs []string `bw:"specs" json:"specs,omitempty"`

	Status         string    `bw:"status"          json:"status"`
	LastError      string    `bw:"last_error"      json:"last_error,omitempty"`
	OperationCount int       `bw:"operation_count" json:"operation_count"`
	LastRefreshAt  time.Time `bw:"last_refresh"    json:"last_refresh_at,omitzero"`
	CreatedAt      time.Time `bw:"created_at"      json:"created_at,omitzero"`

	// Config is opaque provider-owned JSON (spec URL, auth, TLS settings). The
	// registered Provider validates, merges and redacts it.
	Config json.RawMessage `bw:"config" json:"-"`
	// State is an opaque provider watermark (an ETag, a Last-Modified) used to
	// skip re-parsing an unchanged document. Never exposed over the API.
	State json.RawMessage `bw:"state" json:"-"`
}

// OperationOverride corrects one operation's presentation. Every field is
// optional; an empty field leaves the spec's own value in place.
//
// Hidden drops the operation from the catalog entirely — the escape hatch for
// specs that publish internal, deprecated or simply irrelevant endpoints that
// would otherwise be offered to a model as valid choices.
type OperationOverride struct {
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Hidden      bool     `json:"hidden,omitempty"`
}

// EffectiveGroup returns the group a service is listed under, folding the empty
// stored value to GroupDefault.
func (s Service) EffectiveGroup() string {
	if s.Group == "" {
		return GroupDefault
	}

	return s.Group
}

// EffectiveDescription returns what a reader should be shown: the human-written
// description when there is one, otherwise the spec's own summary.
func (s Service) EffectiveDescription() string {
	if s.Description != "" {
		return s.Description
	}

	return s.SpecSummary
}

// EffectiveSpecs returns the cron schedules the scheduler should run for this
// service: the explicit Specs when set, otherwise a single "@every
// <RefreshInterval>". It returns nil when the service has neither
// (manual-only), so the scheduler skips it.
func (s Service) EffectiveSpecs() []string {
	specs := make([]string, 0, len(s.Specs))
	for _, spec := range s.Specs {
		if strings.TrimSpace(spec) != "" {
			specs = append(specs, strings.TrimSpace(spec))
		}
	}
	if len(specs) > 0 {
		return specs
	}

	if s.RefreshInterval > 0 {
		return []string{"@every " + s.RefreshInterval.String()}
	}

	return nil
}

// Operation is one endpoint of a service: an HTTP operation or a gRPC method.
type Operation struct {
	// ID is "<service>/<op_slug>".
	ID      string `bw:"id,pk"          json:"id"`
	Service string `bw:"service,index"  json:"service"`
	// OpSlug is the markdown file name (without .md) inside the service dir.
	OpSlug string `bw:"op_slug" json:"op_slug"`
	// OperationID is the caller-facing handle: the spec's operationId when it
	// has one, otherwise "METHOD /path"; for gRPC, "/pkg.Service/Method". It is
	// what get_api_endpoint takes, and what an override map is keyed by.
	OperationID string `bw:"operation_id,index" json:"operation_id"`

	// Method is an HTTP verb, or MethodGRPC.
	Method string `bw:"method,index" json:"method"`
	// Path is the templated HTTP path ("/v1/invoices/{id}") or the gRPC full
	// method name ("/billing.v1.Invoices/Create").
	Path string `bw:"path" json:"path"`

	Summary     string `bw:"summary"     json:"summary,omitempty"`
	Description string `bw:"description" json:"description,omitempty"`
	// Tags group operations within a service (OpenAPI tags, gRPC service name).
	Tags []string `bw:"tags" json:"tags,omitempty"`
	// TagsNorm holds the lowercase form of every tag, backing case-insensitive
	// tag filtering at the store level. Derived on upsert, never set by
	// callers.
	TagsNorm   []string `bw:"tags_norm,index" json:"-"`
	Deprecated bool     `bw:"deprecated"      json:"deprecated,omitempty"`

	// Detail is the pre-rendered structured payload: parameters, request and
	// response schemas, security and a ready-to-run request recipe.
	//
	// It is rendered at ingest rather than at query time on purpose. Resolving
	// $ref, merging allOf and flattening a schema is the expensive part of
	// serving an endpoint, and a catalog is read far more often than it is
	// refreshed — so the work happens once per sync, and an MCP call is a
	// single key read. It also bounds the cost: the flattening limits are
	// enforced where the data is produced, not hopefully re-applied at every
	// read.
	Detail json.RawMessage `bw:"detail" json:"-"`

	// Hash fingerprints the rendered markdown so unchanged operations skip
	// re-embedding.
	Hash      string    `bw:"hash"       json:"-"`
	UpdatedAt time.Time `bw:"updated_at" json:"updated_at,omitzero"`
}

// RemoteOperation is one operation discovered by a provider, already rendered.
type RemoteOperation struct {
	// OpSlug must be stable across fetches and unique within the service.
	OpSlug      string
	OperationID string
	Method      string
	Path        string
	Summary     string
	Description string
	Tags        []string
	Deprecated  bool
	// Detail is the structured payload stored verbatim on the record.
	Detail json.RawMessage
	// Markdown is the projection indexed into the docs RAG.
	Markdown string
	// Err marks an operation that failed to render; the sync records the error
	// and keeps the previous content.
	Err error
}

// ServiceInfo is the document-level metadata a provider reports alongside the
// operations. The manager copies it onto the Service record after a successful
// sync, leaving human-written fields (Description, BaseURL) alone.
type ServiceInfo struct {
	Title       string
	Version     string
	Summary     string
	ResolvedURL string
}

// FetchResult is the outcome of one sync. Operations are streamed to the emit
// callback during the walk rather than returned here, so a spec of any size
// costs the same memory.
//
// Complete is a guarantee about what was emitted and the only thing that
// licenses deletion: it means every operation the service currently has was
// emitted this run, so a stored record that was not seen is genuinely gone. It
// must be false for any walk cut short.
//
// Unchanged reports that the provider determined the document had not moved
// (a matching ETag) and emitted nothing. The manager then leaves the stored
// operations exactly as they are — an empty, complete sweep would otherwise
// read as "the API has no endpoints" and delete the catalog.
type FetchResult struct {
	Complete  bool
	Unchanged bool
	Info      ServiceInfo
	State     json.RawMessage
}

// Emit receives one discovered operation. Returning an error aborts the fetch,
// which must propagate the error unchanged so the manager can tell a provider
// failure (retry later, do not advance the watermark) from a sink failure.
type Emit func(RemoteOperation) error

// Provider discovers the operations of one service. Implementations live in
// per-kind subpackages. state is the watermark returned by the previous fetch
// (nil on first run and on a forced full refresh).
//
// Operations are handed to emit as they are rendered instead of accumulated: a
// spec with thousands of endpoints then costs the same memory as a small one,
// which is what lets a full sweep run to completion — and only a sweep that
// completes may report Complete.
type Provider interface {
	// Validate checks provider config before a service is persisted.
	Validate(config json.RawMessage) error
	// MergeConfig merges an update with stored config, preserving write-only
	// secrets when the update leaves them blank.
	MergeConfig(current, update json.RawMessage) (json.RawMessage, error)
	// ConfigView returns a JSON-safe, redacted provider config for REST/UI.
	ConfigView(config json.RawMessage) any
	Fetch(ctx context.Context, svc *Service, state json.RawMessage, emit Emit) (*FetchResult, error)
}

// PreviewResult summarizes a read-only provider probe of unsaved config: what
// the document says it is and how many operations it holds, without persisting
// or indexing anything.
type PreviewResult struct {
	Title          string `json:"title,omitempty"`
	Version        string `json:"version,omitempty"`
	Summary        string `json:"summary,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	OperationCount int    `json:"operation_count"`
	// Sample lists a handful of operations so a user can confirm the document
	// is the one they meant before saving.
	Sample []string `json:"sample,omitempty"`
}

// Previewer is an optional provider capability backing the "test config"
// action.
type Previewer interface {
	Preview(ctx context.Context, cfg json.RawMessage, patch json.RawMessage) (PreviewResult, error)
}

// nameRe restricts service and group names to something safe for directories,
// URLs and scope keys.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidName reports whether name is a valid service or group name.
func ValidName(name string) bool { return len(name) <= 100 && nameRe.MatchString(name) }

// NormalizeGroup trims and lowercases a group name, folding the display name
// "default" to the empty stored form so the bucket has one spelling.
func NormalizeGroup(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == GroupDefault {
		return ""
	}

	return name
}

// schemaVersion must be bumped whenever Service, Group or Operation change
// shape.
// v1: initial catalog (groups, services, operations).
const schemaVersion = 1

// Store persists groups, services and operations.
type Store struct {
	groups     *bw.Bucket[Group]
	services   *bw.Bucket[Service]
	operations *bw.Bucket[Operation]
}

// New opens the API-catalog buckets on the given database.
func New(db *bw.DB) (*Store, error) {
	groups, err := bw.RegisterBucket[Group](db, "api_groups", bw.WithVersion[Group](schemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register api_groups bucket; %w", err)
	}

	services, err := bw.RegisterBucket[Service](db, "api_services", bw.WithVersion[Service](schemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register api_services bucket; %w", err)
	}

	operations, err := bw.RegisterBucket[Operation](db, "api_operations", bw.WithVersion[Operation](schemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register api_operations bucket; %w", err)
	}

	return &Store{groups: groups, services: services, operations: operations}, nil
}

// ---- groups ----------------------------------------------------------------

// GroupSummary is one group with its service count and stored description. It
// merges the live tally with the persisted metadata so a group shows up when it
// has services, a description, or both.
type GroupSummary struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	ServiceCount int    `json:"service_count"`
}

// Groups returns the distinct groups with their service counts and
// descriptions, sorted by name. Services with no group fold into GroupDefault;
// groups that only have a description (no services yet) are listed with count 0.
func (s *Store) Groups(ctx context.Context) ([]GroupSummary, error) {
	counts := map[string]int{}

	q := query.New()
	q.AddField("group")
	if err := s.services.Walk(ctx, q, func(svc *Service) error {
		counts[svc.EffectiveGroup()]++

		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan api service groups; %w", err)
	}

	records, err := s.listGroupRecords(ctx)
	if err != nil {
		return nil, err
	}

	descriptions := make(map[string]string, len(records))
	for _, rec := range records {
		name := rec.Name
		if name == "" {
			name = GroupDefault
		}
		descriptions[name] = rec.Description
		if _, ok := counts[name]; !ok {
			counts[name] = 0 // described but empty
		}
	}

	out := make([]GroupSummary, 0, len(counts))
	for name, count := range counts {
		out = append(out, GroupSummary{
			Name:         name,
			Description:  descriptions[name],
			ServiceCount: count,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

func (s *Store) listGroupRecords(ctx context.Context) ([]*Group, error) {
	q, err := query.Parse("_limit=10000")
	if err != nil {
		return nil, fmt.Errorf("parse query; %w", err)
	}

	recs, err := s.groups.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list api group records; %w", err)
	}

	return recs, nil
}

// GetGroup returns the stored record for a group, or nil if none exists.
func (s *Store) GetGroup(ctx context.Context, name string) (*Group, error) {
	rec, err := s.groups.Get(ctx, NormalizeGroup(name))
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get api group %s; %w", name, err)
	}

	return rec, nil
}

// UpsertGroup creates or updates a group's description record, preserving
// CreatedAt across updates.
func (s *Store) UpsertGroup(ctx context.Context, name, description string) (*Group, error) {
	norm := NormalizeGroup(name)
	if norm != "" && !ValidName(norm) {
		return nil, fmt.Errorf("invalid group name %q (want lowercase [a-z0-9._-])", name)
	}

	now := time.Now().UTC()
	rec := &Group{
		Name:        norm,
		Description: strings.TrimSpace(description),
		UpdatedAt:   now,
		CreatedAt:   now,
	}

	existing, err := s.GetGroup(ctx, norm)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		rec.CreatedAt = existing.CreatedAt
	}

	if err := s.groups.Insert(ctx, rec); err != nil {
		return nil, fmt.Errorf("upsert api group %s; %w", norm, err)
	}

	return rec, nil
}

// DeleteGroup removes only the description record; services tagged with the
// group keep their tag, so the group becomes description-less rather than
// empty. This mirrors repo namespaces: deleting metadata must never orphan the
// things it described.
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	if err := s.groups.Delete(ctx, NormalizeGroup(name)); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete api group %s; %w", name, err)
	}

	return nil
}

// ---- services --------------------------------------------------------------

// GetService returns a service by name, or nil if it does not exist.
func (s *Store) GetService(ctx context.Context, name string) (*Service, error) {
	svc, err := s.services.Get(ctx, name)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get api service %s; %w", name, err)
	}

	return svc, nil
}

// ListServices returns all services sorted by name.
func (s *Store) ListServices(ctx context.Context) ([]*Service, error) {
	q, err := query.Parse("_limit=100000")
	if err != nil {
		return nil, fmt.Errorf("parse query; %w", err)
	}

	services, err := s.services.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list api services; %w", err)
	}

	if services == nil {
		services = []*Service{}
	}

	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	return services, nil
}

// servicesWhere builds the filter for a service listing. An empty group matches
// every group; GroupDefault matches the unset bucket. search is a
// case-insensitive substring of the name.
func servicesWhere(group, search string) []query.Expression {
	var where []query.Expression

	if group != "" {
		where = append(where,
			query.NewExpressionCmp(query.OperatorEq, "group", NormalizeGroup(group)).Expression())
	}
	if search = strings.TrimSpace(search); search != "" {
		where = append(where,
			query.NewExpressionCmp(query.OperatorILike, "name", "%"+search+"%").Expression())
	}

	return where
}

// ServicesPaged returns one page of services matching the filters, sorted by
// name, together with the total count of matching records.
func (s *Store) ServicesPaged(ctx context.Context, group, search string, offset, limit int) ([]*Service, int, error) {
	countQuery := query.New()
	countQuery.Where = servicesWhere(group, search)
	count, err := s.services.Count(ctx, countQuery)
	if err != nil {
		return nil, 0, fmt.Errorf("count api services; %w", err)
	}
	total := int(count)

	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || offset >= total {
		return []*Service{}, total, nil
	}

	q := query.New()
	q.Where = servicesWhere(group, search)
	q.Sort = []query.ExpressionSort{{Field: "name"}}
	q.SetOffset(uint64(offset))
	q.SetLimit(uint64(limit))

	services, err := s.services.Find(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("list api services; %w", err)
	}

	if services == nil {
		services = []*Service{}
	}

	return services, total, nil
}

// UpsertService inserts or replaces a service record.
func (s *Store) UpsertService(ctx context.Context, svc *Service) error {
	if err := s.services.Insert(ctx, svc); err != nil {
		return fmt.Errorf("upsert api service %s; %w", svc.Name, err)
	}

	return nil
}

// DeleteService removes a service record and all its operation records.
func (s *Store) DeleteService(ctx context.Context, name string) error {
	ops, err := s.Operations(ctx, name)
	if err != nil {
		return err
	}

	for _, op := range ops {
		if err := s.DeleteOperation(ctx, op.ID); err != nil {
			return err
		}
	}

	if err := s.services.Delete(ctx, name); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete api service %s; %w", name, err)
	}

	return nil
}

// ---- operations ------------------------------------------------------------

// Operations returns every operation record of one service sorted by slug.
func (s *Store) Operations(ctx context.Context, service string) ([]*Operation, error) {
	q := query.New()
	q.Where = append(q.Where,
		query.NewExpressionCmp(query.OperatorEq, "service", service).Expression())
	q.SetLimit(100000)

	ops, err := s.operations.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list operations of %s; %w", service, err)
	}

	if ops == nil {
		ops = []*Operation{}
	}

	sort.Slice(ops, func(i, j int) bool { return ops[i].OpSlug < ops[j].OpSlug })

	return ops, nil
}

// operationsWhere builds the filter for an operation listing. search matches
// the path, the summary or the operation id; tag and method are exact
// (case-insensitive) filters.
func operationsWhere(service, search, tag, method string) []query.Expression {
	where := []query.Expression{
		query.NewExpressionCmp(query.OperatorEq, "service", service).Expression(),
	}

	if tag = strings.ToLower(strings.TrimSpace(tag)); tag != "" {
		where = append(where,
			query.NewExpressionCmp(query.OperatorJIn, "tags_norm", []string{tag}).Expression())
	}
	if method = strings.ToUpper(strings.TrimSpace(method)); method != "" {
		where = append(where,
			query.NewExpressionCmp(query.OperatorEq, "method", method).Expression())
	}
	if search = strings.TrimSpace(search); search != "" {
		// The service filter above is an index seek, so this OR only ever
		// scans one service's operations — the same trade websource makes for
		// its title filter. bw's planner has no plan for LIKE, and pushing a
		// bounded scan down still beats loading the service into memory.
		where = append(where, query.NewExpressionLogic(query.OperatorOr, []query.Expression{
			query.NewExpressionCmp(query.OperatorILike, "path", "%"+search+"%").Expression(),
			query.NewExpressionCmp(query.OperatorILike, "summary", "%"+search+"%").Expression(),
			query.NewExpressionCmp(query.OperatorILike, "operation_id", "%"+search+"%").Expression(),
		}).Expression())
	}

	return where
}

// OperationsPaged returns one page of a service's operations matching the
// filters, sorted by path then method, with the total count. Filtering and
// paging happen at the store level so a spec with thousands of endpoints is
// never loaded into memory to answer one listing.
func (s *Store) OperationsPaged(ctx context.Context, service, search, tag, method string, offset, limit int) ([]*Operation, int, error) {
	countQuery := query.New()
	countQuery.Where = operationsWhere(service, search, tag, method)
	count, err := s.operations.Count(ctx, countQuery)
	if err != nil {
		return nil, 0, fmt.Errorf("count operations of %s; %w", service, err)
	}
	total := int(count)

	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || offset >= total {
		return []*Operation{}, total, nil
	}

	q := query.New()
	q.Where = operationsWhere(service, search, tag, method)
	q.Sort = []query.ExpressionSort{{Field: "path"}, {Field: "method"}}
	q.SetOffset(uint64(offset))
	q.SetLimit(uint64(limit))

	ops, err := s.operations.Find(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("list operations of %s; %w", service, err)
	}

	if ops == nil {
		ops = []*Operation{}
	}

	return ops, total, nil
}

// GetOperation returns an operation by record id, or nil if it does not exist.
func (s *Store) GetOperation(ctx context.Context, id string) (*Operation, error) {
	op, err := s.operations.Get(ctx, id)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get operation %s; %w", id, err)
	}

	return op, nil
}

// FindOperation resolves a caller-supplied handle within one service. It
// accepts the operation id first, then falls back to the slug and to
// "METHOD /path", because those are the three spellings a reader plausibly has
// in hand: the one listings print, the one file paths use, and the one the spec
// document shows.
func (s *Store) FindOperation(ctx context.Context, service, handle string) (*Operation, error) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return nil, nil
	}

	q := query.New()
	q.Where = []query.Expression{
		query.NewExpressionCmp(query.OperatorEq, "service", service).Expression(),
		query.NewExpressionCmp(query.OperatorEq, "operation_id", handle).Expression(),
	}
	q.SetLimit(1)

	ops, err := s.operations.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("find operation %s of %s; %w", handle, service, err)
	}
	if len(ops) > 0 {
		return ops[0], nil
	}

	if op, err := s.GetOperation(ctx, OperationID(service, handle)); err != nil {
		return nil, err
	} else if op != nil {
		return op, nil
	}

	return s.GetOperation(ctx, OperationID(service, Slugify(handle)))
}

// UpsertOperation inserts or replaces an operation record, deriving TagsNorm so
// tag filtering runs at the store level, case-insensitively.
func (s *Store) UpsertOperation(ctx context.Context, op *Operation) error {
	op.TagsNorm = normalizeTags(op.Tags)

	if err := s.operations.Insert(ctx, op); err != nil {
		return fmt.Errorf("upsert operation %s; %w", op.ID, err)
	}

	return nil
}

// DeleteOperation removes an operation record.
func (s *Store) DeleteOperation(ctx context.Context, id string) error {
	if err := s.operations.Delete(ctx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete operation %s; %w", id, err)
	}

	return nil
}

// Tags returns the distinct tags across one service's operations, sorted,
// preserving the casing of the first occurrence. It backs the UI and MCP tag
// filters.
func (s *Store) Tags(ctx context.Context, service string) ([]string, error) {
	q := query.New()
	q.Where = append(q.Where,
		query.NewExpressionCmp(query.OperatorEq, "service", service).Expression())
	q.AddField("tags")
	q.SetLimit(100000)

	seen := map[string]string{} // lowercase -> original casing
	if err := s.operations.Walk(ctx, q, func(op *Operation) error {
		for _, t := range op.Tags {
			if t = strings.TrimSpace(t); t == "" {
				continue
			}
			if _, ok := seen[strings.ToLower(t)]; !ok {
				seen[strings.ToLower(t)] = t
			}
		}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan tags of %s; %w", service, err)
	}

	out := make([]string, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})

	return out, nil
}

// normalizeTags returns the lowercased, trimmed, de-duplicated tags, dropping
// empties. Returns nil when there are none so the stored field stays absent.
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// ---- identity and filesystem safety ----------------------------------------

// OperationID builds the primary key of an operation record.
func OperationID(service, slug string) string { return service + "/" + slug }

// slugRe is what a slug may contain. It is deliberately narrower than what
// Slugify produces so that a slug is always a single, self-contained path
// element: no separator, no dot segment, no leading dash.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidSlug reports whether a slug is safe as an operation identity and as a
// filename.
//
// A slug reaches the filesystem as "<slug>.md" under the service's directory
// and reaches the store as "<service>/<slug>", and it originates from an
// untrusted document: operationId is free text controlled by whoever publishes
// the spec. filepath.Join resolves ".." instead of rejecting it, so an
// unchecked slug turns an operation delete into a delete of any *.md file the
// process can reach. Slugify is safe by construction, but the guard lives here,
// at the point where the slug is trusted.
func ValidSlug(slug string) bool {
	return slug != "" && len(slug) <= 200 && slug != "." && slug != ".." && slugRe.MatchString(slug)
}

// OperationFile returns the path of a slug's markdown inside dir, refusing any
// slug that is not a safe, single path element. Every read, write and delete of
// an operation's markdown must go through it.
func OperationFile(dir, slug string) (string, error) {
	if !ValidSlug(slug) {
		return "", fmt.Errorf("unsafe operation slug %q", slug)
	}

	return filepath.Join(dir, slug+".md"), nil
}
