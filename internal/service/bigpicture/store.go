// Package bigpicture stores cross-repository architecture workspaces and
// immutable, multi-document publications outside git clones.
package bigpicture

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/worldline-go/hardloop"
)

const (
	MaxDocuments        = 64
	MaxDocumentBytes    = 256 << 10
	MaxPublicationBytes = 4 << 20
	MaxRevisions        = 10
)

var (
	ErrInvalid               = errors.New("invalid big picture")
	ErrNotFound              = errors.New("big picture or document not found")
	ErrConflict              = errors.New("big picture changed; reload before saving or publishing")
	ErrExists                = errors.New("big picture already exists")
	ErrGenerationUnavailable = errors.New("enable documentation generation and configure a chat model first")
	namePattern              = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	segmentPattern           = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	storagePattern           = regexp.MustCompile(`^[a-zA-Z0-9]{26}$`)
)

// Sources are references only. Selecting a connection does not grant tools or
// read resources; execution must re-check the connection's current allowlist.
type Source struct {
	Kind string `bw:"kind" json:"kind"`
	Ref  string `bw:"ref" json:"ref"`
}

type Config struct {
	Name            string   `json:"name"`
	Title           string   `json:"title"`
	Namespace       string   `json:"namespace"`
	Description     string   `json:"description"`
	Prompt          string   `json:"prompt"`
	Sources         []Source `json:"sources"`
	ExpectedVersion uint64   `json:"expected_version"`
	// Nil keeps the saved schedule so partial clients cannot silently disable
	// it; an empty list disables scheduling.
	Schedule *[]string `json:"schedule,omitempty"`
}

// Run records the latest automatic research outcome, including runs that did
// not publish, so failures and no-op checks are visible on the workspace.
type Run struct {
	Trigger       string    `bw:"trigger" json:"trigger"`
	Status        string    `bw:"status" json:"status"`
	Message       string    `bw:"message" json:"message,omitempty"`
	At            time.Time `bw:"at" json:"at"`
	ConfigVersion uint64    `bw:"config_version" json:"config_version"`
	// Revision is the publication the run compared against or produced.
	Revision     string `bw:"revision" json:"revision,omitempty"`
	ResearchHash string `bw:"research_hash" json:"-"`
}

const (
	RunPublished = "published"
	RunUnchanged = "unchanged" // collected evidence matched; model skipped
	RunNoChanges = "no_changes"
	RunFailed    = "failed"
)

type Revision struct {
	ID            string    `bw:"id" json:"id"`
	ConfigVersion uint64    `bw:"config_version" json:"config_version"`
	DocumentCount int       `bw:"document_count" json:"document_count"`
	PublishedAt   time.Time `bw:"published_at" json:"published_at"`
	Producer      string    `bw:"producer" json:"producer"`
	ChangeSummary string    `bw:"change_summary" json:"change_summary,omitempty"`
	ResearchHash  string    `bw:"research_hash" json:"research_hash,omitempty"`
}

type Picture struct {
	Name            string   `bw:"name,pk" json:"name"`
	StorageID       string   `bw:"storage_id" json:"-"` // random instance id; never a user path
	Title           string   `bw:"title" json:"title"`
	Namespace       string   `bw:"namespace,index" json:"namespace"`
	Description     string   `bw:"description" json:"description"`
	Prompt          string   `bw:"prompt" json:"prompt"`
	Sources         []Source `bw:"sources" json:"sources"`
	Version         uint64   `bw:"version" json:"version"`
	CurrentRevision string   `bw:"current_revision" json:"current_revision"`
	TextRevision    string   `bw:"text_revision" json:"text_revision"`
	VectorRevision  string   `bw:"vector_revision" json:"vector_revision"`
	TextIndex       string   `bw:"text_index" json:"-"`
	VectorIndex     string   `bw:"vector_index" json:"-"`
	// IndexPending tracks index keys that may exist but are not live, so an
	// interrupted indexing attempt is reclaimed by the next one.
	IndexPending []string   `bw:"index_pending" json:"-"`
	Schedule     []string   `bw:"schedule" json:"schedule"`
	LastRun      *Run       `bw:"last_run" json:"last_run,omitempty"`
	Revisions    []Revision `bw:"revisions" json:"revisions"`
	CreatedAt    time.Time  `bw:"created_at" json:"created_at"`
	UpdatedAt    time.Time  `bw:"updated_at" json:"updated_at"`
}

type Summary struct {
	Name            string    `json:"name"`
	Title           string    `json:"title"`
	Namespace       string    `json:"namespace"`
	Description     string    `json:"description"`
	Version         uint64    `json:"version"`
	SourceCount     int       `json:"source_count"`
	CurrentRevision string    `json:"current_revision"`
	DocumentCount   int       `json:"document_count"`
	Stale           bool      `json:"stale"` // publication uses an older workspace config
	UpdatedAt       time.Time `json:"updated_at"`
}

type ListOptions struct {
	Namespace, Query string
	Page, PerPage    int
}
type Page struct {
	Items   []Summary `json:"items"`
	Total   int       `json:"total"`
	Page    int       `json:"page"`
	PerPage int       `json:"per_page"`
}

// Evidence records a publisher's source citation. It is a claimed reference,
// not independent validation by Krabby or a guarantee of current live state.
type Evidence struct {
	Source   Source `json:"source"`
	Locator  string `json:"locator"` // repository path, original URL, or MCP URI/tool
	Revision string `json:"revision,omitempty"`
}

type Document struct {
	Path     string     `json:"path"`
	Title    string     `json:"title"`
	Markdown string     `json:"markdown"`
	Evidence []Evidence `json:"evidence"`
}

type DocMeta struct {
	Path     string     `json:"path"`
	Title    string     `json:"title"`
	Bytes    int        `json:"bytes"`
	Hash     string     `json:"hash"`
	Evidence []Evidence `json:"evidence"`
}

type Publication struct {
	ExpectedInstance string     `json:"-"`
	ExpectedVersion  uint64     `json:"expected_version"`
	ExpectedRevision string     `json:"expected_revision"`
	Producer         string     `json:"producer"`
	Overview         string     `json:"overview"`
	Documents        []Document `json:"documents"`
	ChangeSummary    string     `json:"change_summary,omitempty"`
	ResearchHash     string     `json:"-"`
}

type Snapshot struct {
	Revision
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	Namespace string    `json:"namespace"`
	Prompt    string    `json:"prompt"`
	Sources   []Source  `json:"sources"`
	Overview  string    `json:"overview"`
	Documents []DocMeta `json:"documents"`
}

type DocumentRead struct {
	*repofs.FileContent
	Revision string     `json:"revision"`
	Title    string     `json:"title"`
	Evidence []Evidence `json:"evidence"`
}

type Store struct {
	bucket *bw.Bucket[Picture]
	root   string
	mu     sync.Mutex // serializes config changes, publication pointers and deletion
}

func New(db *bw.DB, root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("big picture root directory required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	bucket, err := bw.RegisterBucket[Picture](db, "big_pictures", bw.WithVersion[Picture](1))
	if err != nil {
		return nil, err
	}
	return &Store{bucket: bucket, root: root}, nil
}

func NormalizeNamespace(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "default"
	}
	return value
}

func NormalizeConfig(cfg Config) (Config, error) {
	if cfg.Schedule != nil {
		schedule := slices.Clone(*cfg.Schedule)
		if len(schedule) > 10 {
			return cfg, fmt.Errorf("%w: at most 10 cron specifications", ErrInvalid)
		}
		for i := range schedule {
			schedule[i] = strings.TrimSpace(schedule[i])
			if schedule[i] == "" || len(schedule[i]) > 256 {
				return cfg, fmt.Errorf("%w: invalid cron specification", ErrInvalid)
			}
		}
		if len(schedule) > 0 {
			if _, err := hardloop.NewCron(hardloop.Cron{Name: "bigpicture", Specs: schedule}); err != nil {
				return cfg, fmt.Errorf("%w: invalid cron schedule", ErrInvalid)
			}
		}
		if schedule == nil {
			schedule = []string{}
		}
		cfg.Schedule = &schedule
	}
	cfg.Name, cfg.Title = strings.TrimSpace(cfg.Name), strings.TrimSpace(cfg.Title)
	cfg.Namespace = NormalizeNamespace(cfg.Namespace)
	cfg.Description, cfg.Prompt = strings.TrimSpace(cfg.Description), strings.TrimSpace(cfg.Prompt)
	if !namePattern.MatchString(cfg.Name) || !namePattern.MatchString(cfg.Namespace) || cfg.Title == "" || len(cfg.Title) > 256 || len(cfg.Description) > 4096 || cfg.Prompt == "" || len(cfg.Prompt) > 32768 {
		return cfg, fmt.Errorf("%w: valid name/namespace, title and prompt are required (title ≤256, description ≤4096, prompt ≤32768 bytes)", ErrInvalid)
	}
	if len(cfg.Sources) == 0 || len(cfg.Sources) > 50 {
		return cfg, fmt.Errorf("%w: select between 1 and 50 sources", ErrInvalid)
	}
	sources := make([]Source, 0, len(cfg.Sources))
	seen := map[Source]bool{}
	for _, source := range cfg.Sources {
		source.Ref = strings.TrimSpace(source.Ref)
		switch source.Kind {
		case "repo", "web", "api", "mcp":
		default:
			return cfg, fmt.Errorf("%w: unknown source kind", ErrInvalid)
		}
		if source.Ref == "" || len(source.Ref) > 2048 || strings.ContainsAny(source.Ref, "\r\n\x00") {
			return cfg, fmt.Errorf("%w: invalid source reference", ErrInvalid)
		}
		if !seen[source] {
			sources = append(sources, source)
			seen[source] = true
		}
	}
	slices.SortFunc(sources, func(a, b Source) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	cfg.Sources = sources
	data, err := json.Marshal(cfg)
	if err != nil || len(data) > 64<<10 {
		return cfg, fmt.Errorf("%w: configuration exceeds 64 KiB", ErrInvalid)
	}
	return cfg, nil
}

func (s *Store) Get(ctx context.Context, name string) (*Picture, error) {
	p, err := s.bucket.Get(ctx, name)
	if errors.Is(err, bw.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err == nil && !storagePattern.MatchString(p.StorageID) {
		return nil, errors.New("invalid big picture storage identity")
	}
	if err == nil {
		for _, revision := range p.Revisions {
			if !storagePattern.MatchString(revision.ID) {
				return nil, errors.New("invalid big picture revision identity")
			}
		}
	}
	if err == nil && p.Revisions == nil {
		p.Revisions = []Revision{}
	}
	if err == nil && p.Schedule == nil {
		p.Schedule = []string{}
	}
	return p, err
}

func (s *Store) Save(ctx context.Context, name string, cfg Config) (*Picture, error) {
	cfg, err := NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var p *Picture
	if name == "" {
		if _, err := s.Get(ctx, cfg.Name); err == nil {
			return nil, ErrExists
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if cfg.ExpectedVersion != 0 {
			return nil, ErrConflict
		}
		p = &Picture{Name: cfg.Name, StorageID: rand.Text(), CreatedAt: time.Now().UTC(), Revisions: []Revision{}}
	} else {
		p, err = s.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if cfg.Name != name {
			return nil, fmt.Errorf("%w: name cannot change", ErrInvalid)
		}
		if p.Version != cfg.ExpectedVersion {
			return nil, ErrConflict
		}
	}
	p.Title, p.Namespace, p.Description, p.Prompt, p.Sources = cfg.Title, cfg.Namespace, cfg.Description, cfg.Prompt, cfg.Sources
	if cfg.Schedule != nil {
		p.Schedule = *cfg.Schedule
	}
	if p.Schedule == nil {
		p.Schedule = []string{}
	}
	p.Version++
	p.UpdatedAt = time.Now().UTC()
	if err := s.bucket.Insert(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) List(ctx context.Context, opts ListOptions) (Page, error) {
	page, perPage := max(1, opts.Page), min(100, max(1, opts.PerPage))
	if opts.PerPage <= 0 {
		perPage = 20
	}
	result := Page{Items: []Summary{}, Page: page, PerPage: perPage}
	ns, text := NormalizeNamespace(opts.Namespace), strings.ToLower(strings.TrimSpace(opts.Query))
	q := query.New()
	if ns != "*" {
		q.Where = append(q.Where, query.NewExpressionCmp(query.OperatorEq, "namespace", ns).Expression())
	}
	if text != "" {
		q.Where = append(q.Where, query.NewExpressionLogic(query.OperatorOr, []query.Expression{
			query.NewExpressionCmp(query.OperatorILike, "name", "%"+text+"%").Expression(),
			query.NewExpressionCmp(query.OperatorILike, "title", "%"+text+"%").Expression(),
			query.NewExpressionCmp(query.OperatorILike, "description", "%"+text+"%").Expression(),
		}).Expression())
	}
	count, err := s.bucket.Count(ctx, q)
	if err != nil {
		return Page{}, err
	}
	result.Total = int(count)
	// Bound the offset before multiplication, avoiding hostile page overflow.
	if page > (result.Total+perPage-1)/perPage {
		return result, nil
	}
	q.Sort = []query.ExpressionSort{{Field: "name"}}
	q.SetOffset(uint64((page - 1) * perPage))
	q.SetLimit(uint64(perPage))
	items, err := s.bucket.Find(ctx, q)
	if err != nil {
		return Page{}, err
	}
	for _, p := range items {
		summary := Summary{Name: p.Name, Title: p.Title, Namespace: p.Namespace, Description: p.Description, Version: p.Version, SourceCount: len(p.Sources), CurrentRevision: p.CurrentRevision, UpdatedAt: p.UpdatedAt}
		for _, revision := range p.Revisions {
			if revision.ID == p.CurrentRevision {
				summary.DocumentCount = revision.DocumentCount
				summary.Stale = revision.ConfigVersion != p.Version
			}
		}
		result.Items = append(result.Items, summary)
	}
	return result, nil
}

func ValidDocumentPath(value string) bool {
	if len(value) > 256 || !strings.HasSuffix(value, ".md") || path.Clean(value) != value || strings.HasPrefix(value, "/") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if !segmentPattern.MatchString(segment) {
			return false
		}
	}
	return true
}

func ValidatePublication(p *Picture, pub Publication) error {
	if len(pub.ChangeSummary) > 4096 {
		return fmt.Errorf("%w: change summary exceeds size limit", ErrInvalid)
	}
	if pub.ExpectedInstance != "" && pub.ExpectedInstance != p.StorageID {
		return ErrConflict
	}
	if pub.ExpectedVersion != p.Version || pub.ExpectedRevision != p.CurrentRevision {
		return ErrConflict
	}
	if len(pub.Documents) == 0 || len(pub.Documents) > MaxDocuments || pub.Producer == "" || len(pub.Producer) > 256 {
		return fmt.Errorf("%w: producer and 1–64 documents required", ErrInvalid)
	}
	seen := map[string]bool{}
	sources := map[Source]bool{}
	for _, source := range p.Sources {
		sources[source] = true
	}
	total := 0
	for _, doc := range pub.Documents {
		if !ValidDocumentPath(doc.Path) || seen[doc.Path] || strings.TrimSpace(doc.Title) == "" || len(doc.Title) > 256 || strings.TrimSpace(doc.Markdown) == "" || len(doc.Markdown) > MaxDocumentBytes || !utf8.ValidString(doc.Markdown) || len(doc.Evidence) > 50 {
			return fmt.Errorf("%w: invalid or duplicate document, title, content or evidence", ErrInvalid)
		}
		// A file cannot also serve as a directory for another document.
		for existing := range seen {
			if strings.HasPrefix(existing, doc.Path+"/") || strings.HasPrefix(doc.Path, existing+"/") {
				return fmt.Errorf("%w: conflicting document paths", ErrInvalid)
			}
		}
		seen[doc.Path] = true
		for _, evidence := range doc.Evidence {
			if !sources[evidence.Source] || strings.TrimSpace(evidence.Locator) == "" || len(evidence.Locator) > 2048 || len(evidence.Revision) > 256 {
				return fmt.Errorf("%w: evidence must reference a selected source with a locator", ErrInvalid)
			}
		}
		total += len(doc.Markdown)
	}
	if total > MaxPublicationBytes || !seen[pub.Overview] {
		return fmt.Errorf("%w: overview must name a document; total content must not exceed 4 MiB", ErrInvalid)
	}
	meta, _ := json.Marshal(pub)
	if len(meta) > 8<<20 {
		return fmt.Errorf("%w: publication metadata exceeds size limit", ErrInvalid)
	}
	metadata := pub
	metadata.Documents = slices.Clone(pub.Documents)
	for i := range metadata.Documents {
		metadata.Documents[i].Markdown = ""
	}
	meta, _ = json.Marshal(metadata)
	if len(meta) > 512<<10 {
		return fmt.Errorf("%w: publication metadata exceeds 512 KiB", ErrInvalid)
	}
	return nil
}

// Publish stages a complete immutable directory, then atomically switches one
// state record. A failed write never changes the current publication pointer.
// Explicit version/revision preconditions prevent stale producers overwriting
// newer configuration or publications.
func (s *Store) Publish(ctx context.Context, name string, pub Publication) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := ValidatePublication(p, pub); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	revision := Revision{ID: rand.Text(), ConfigVersion: p.Version, DocumentCount: len(pub.Documents), PublishedAt: time.Now().UTC(), Producer: pub.Producer, ChangeSummary: pub.ChangeSummary, ResearchHash: pub.ResearchHash}
	dir := path.Join(p.StorageID, revision.ID)
	published := false
	defer func() {
		if !published {
			_ = root.RemoveAll(dir)
		}
	}()
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	snapshot := &Snapshot{Revision: revision, Name: p.Name, Title: p.Title, Namespace: p.Namespace, Prompt: p.Prompt, Sources: p.Sources, Overview: pub.Overview, Documents: []DocMeta{}}
	for _, doc := range pub.Documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := root.MkdirAll(path.Join(dir, path.Dir(doc.Path)), 0o700); err != nil {
			return nil, err
		}
		if err := writeSynced(root, path.Join(dir, doc.Path), []byte(doc.Markdown)); err != nil {
			return nil, err
		}
		hash := sha256.Sum256([]byte(doc.Markdown))
		evidence := slices.Clone(doc.Evidence)
		if evidence == nil {
			evidence = []Evidence{}
		}
		snapshot.Documents = append(snapshot.Documents, DocMeta{Path: doc.Path, Title: doc.Title, Bytes: len(doc.Markdown), Hash: hex.EncodeToString(hash[:]), Evidence: evidence})
	}
	slices.SortFunc(snapshot.Documents, func(a, b DocMeta) int { return strings.Compare(a.Path, b.Path) })
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	if err := writeSynced(root, path.Join(dir, "manifest.json"), data); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.CurrentRevision = revision.ID
	p.TextRevision, p.VectorRevision = "", ""
	p.Revisions = append([]Revision{revision}, p.Revisions...)
	var expired []Revision
	if len(p.Revisions) > MaxRevisions {
		expired = p.Revisions[MaxRevisions:]
		p.Revisions = p.Revisions[:MaxRevisions]
	}
	p.UpdatedAt = revision.PublishedAt
	if err := s.bucket.Insert(ctx, p); err != nil {
		return nil, err
	}
	published = true
	// Unreferenced directories cannot be read through the API. Cleanup failure
	// is non-fatal and never retracts a successfully published snapshot.
	for _, old := range expired {
		if err := root.RemoveAll(path.Join(p.StorageID, old.ID)); err != nil {
			slog.Warn("clean expired big picture publication", "name", name)
		}
	}
	return snapshot, nil
}

func writeSynced(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if n, err := f.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	return f.Sync()
}

func (s *Store) Snapshot(ctx context.Context, name, revision string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(ctx, name, revision)
}

func (s *Store) snapshot(ctx context.Context, name, revision string) (*Snapshot, error) {
	p, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if revision == "" {
		revision = p.CurrentRevision
	}
	if !slices.ContainsFunc(p.Revisions, func(r Revision) bool { return r.ID == revision }) {
		return nil, ErrNotFound
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(path.Join(p.StorageID, revision, "manifest.json"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("big picture manifest exceeds read limit")
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.ID != revision || snapshot.Name != name {
		return nil, errors.New("big picture manifest identity mismatch")
	}
	return &snapshot, nil
}

func (s *Store) ReadDocument(ctx context.Context, name, revision, document string, offset int64, maxBytes int) (*DocumentRead, error) {
	if offset < 0 || (maxBytes > 0 && maxBytes < 4) {
		return nil, fmt.Errorf("%w: offset must be non-negative and max_bytes at least 4", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, err := s.snapshot(ctx, name, revision)
	if err != nil {
		return nil, err
	}
	var meta *DocMeta
	for i := range snapshot.Documents {
		if snapshot.Documents[i].Path == document {
			meta = &snapshot.Documents[i]
			break
		}
	}
	if meta == nil || !ValidDocumentPath(document) {
		return nil, ErrNotFound
	}
	p, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = 32768
	}
	maxBytes = min(maxBytes, 128<<10)
	content, err := repofs.ReadFile(s.root, path.Join(p.StorageID, snapshot.ID, document), offset, maxBytes)
	if err != nil {
		return nil, err
	}
	content.Path = document
	// Never replace a split UTF-8 rune with U+FFFD in JSON; continuation offsets
	// are the actual returned byte count, not a nominal page size.
	for i := 0; i < 3 && content.Truncated && !utf8.ValidString(content.Content); i++ {
		content.Content = content.Content[:len(content.Content)-1]
	}
	if !utf8.ValidString(content.Content) {
		return nil, fmt.Errorf("%w: offset splits a UTF-8 character", ErrInvalid)
	}
	content.Bytes = len(content.Content)
	content.Truncated = offset+int64(content.Bytes) < content.TotalSize
	return &DocumentRead{FileContent: content, Revision: snapshot.ID, Title: meta.Title, Evidence: meta.Evidence}, nil
}

func (s *Store) Delete(ctx context.Context, name string, expectedVersion uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	if p.Version != expectedVersion {
		return ErrConflict
	}
	if err := s.bucket.Delete(ctx, name); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.root)
	if err == nil {
		defer func() { _ = root.Close() }()
		err = root.RemoveAll(p.StorageID)
	}
	if err != nil {
		slog.Warn("clean deleted big picture data", "name", name)
	}
	return nil
}
