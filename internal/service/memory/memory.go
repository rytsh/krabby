// Package memory stores durable, shared agent memories: decisions,
// conventions and gotchas that agents and people want to carry across
// sessions. Every memory lives in a space (a free-form grouping label such as
// "default", "team-payments" or "agent:claude-code"); spaces organise, they do
// not restrict access.
//
// A memory may pin evidence in tracked repositories. The pinned lines are
// hashed at write time so later repository refreshes can tell whether the code
// a memory talks about still says what it said, moved, or is gone.
package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxTitleBytes   = 200
	MaxContentBytes = 16 << 10
	MaxTags         = 20
	MaxTagBytes     = 64
	MaxEvidence     = 10
	MaxHistory      = 50
	MaxNoteBytes    = 512
	MaxAuthorBytes  = 128
)

const DefaultSpace = "default"

// Kinds classify what a memory records. A fixed set keeps filtering and the UI
// predictable; free text belongs in tags.
const (
	KindDecision   = "decision"
	KindConvention = "convention"
	KindGotcha     = "gotcha"
	KindFact       = "fact"
	KindPreference = "preference"
	KindTodo       = "todo"
	KindNote       = "note"
)

var Kinds = []string{KindDecision, KindConvention, KindGotcha, KindFact, KindPreference, KindTodo, KindNote}

// Memory statuses. Expired is derived at read time from ExpiresAt and never
// stored.
const (
	StatusActive   = "active"
	StatusStale    = "stale"    // pinned code changed
	StatusOrphaned = "orphaned" // pinned file or repository is gone
	StatusExpired  = "expired"
)

// Evidence check outcomes.
const (
	EvidenceOK      = "ok"
	EvidenceMoved   = "moved" // same lines found elsewhere in the file
	EvidenceChanged = "changed"
	EvidenceMissing = "missing"
)

// History actions.
const (
	ActionCreated  = "created"
	ActionUpdated  = "updated"
	ActionStale    = "stale"
	ActionOrphaned = "orphaned"
	ActionRestored = "restored"
	ActionMoved    = "moved"
)

var (
	ErrInvalid  = errors.New("invalid memory")
	ErrNotFound = errors.New("memory not found")
	ErrConflict = errors.New("memory changed; reload before updating")
	ErrSecret   = errors.New("memory content looks like it contains a secret; store a reference instead")

	spacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,127}$`)
	idPattern    = regexp.MustCompile(`^[A-Z2-7]{26}$`)
)

// Evidence pins a memory to a location in a tracked repository. Repo and
// either Path or Symbol are required; a symbol without lines is resolved to its
// definition. Commit, Hash and Status are maintained by Krabby, not the caller.
type Evidence struct {
	Repo      string `bw:"repo" json:"repo"`
	Path      string `bw:"path" json:"path"`
	Symbol    string `bw:"symbol" json:"symbol,omitempty"`
	StartLine int    `bw:"start_line" json:"start_line,omitempty"`
	EndLine   int    `bw:"end_line" json:"end_line,omitempty"`
	Commit    string `bw:"commit" json:"commit,omitempty"`
	Hash      string `bw:"hash" json:"-"`
	Status    string `bw:"status" json:"status,omitempty"`
}

// Event is one entry of a memory's history.
type Event struct {
	At     time.Time `bw:"at" json:"at"`
	Action string    `bw:"action" json:"action"`
	Actor  string    `bw:"actor" json:"actor,omitempty"`
	Note   string    `bw:"note" json:"note,omitempty"`
}

// Memory is one stored record. Content is Markdown.
type Memory struct {
	ID        string     `bw:"id,pk" json:"id"`
	Space     string     `bw:"space,index" json:"space"`
	Kind      string     `bw:"kind,index" json:"kind"`
	Status    string     `bw:"status,index" json:"status"`
	Title     string     `bw:"title,fts" json:"title"`
	Content   string     `bw:"content,fts" json:"content"`
	Tags      []string   `bw:"tags" json:"tags"`
	Evidence  []Evidence `bw:"evidence" json:"evidence"`
	Author    string     `bw:"author" json:"author,omitempty"`
	Version   uint64     `bw:"version" json:"version"`
	CreatedAt time.Time  `bw:"created_at" json:"created_at"`
	UpdatedAt time.Time  `bw:"updated_at" json:"updated_at"`
	ExpiresAt time.Time  `bw:"expires_at" json:"expires_at,omitzero"`
	History   []Event    `bw:"history" json:"history"`

	// SearchText folds tags, paths and symbols into the full-text index so a
	// search for a file name or tag finds the memory.
	SearchText string `bw:"search_text,fts" json:"-"`
	// UpdatedUnix orders listings; bw sorts scalars, not time.Time.
	UpdatedUnix int64 `bw:"updated_unix,index" json:"-"`
	// ContentHash detects exact duplicates within a space.
	ContentHash string `bw:"content_hash,index" json:"-"`
}

// Input is the caller-controlled part of a memory, used by create and update.
type Input struct {
	Space     string     `json:"space"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Content   string     `json:"content"`
	Tags      []string   `json:"tags"`
	Evidence  []Evidence `json:"evidence"`
	ExpiresAt time.Time  `json:"expires_at,omitzero"`
	// Author is free text naming who wrote it (a person, team or agent);
	// optional and never derived from the connection.
	Author string `json:"author,omitempty"`
}

// Patch updates selected fields; nil keeps the stored value.
type Patch struct {
	ExpectedVersion uint64      `json:"expected_version"`
	Space           *string     `json:"space,omitempty"`
	Kind            *string     `json:"kind,omitempty"`
	Title           *string     `json:"title,omitempty"`
	Content         *string     `json:"content,omitempty"`
	Tags            *[]string   `json:"tags,omitempty"`
	Evidence        *[]Evidence `json:"evidence,omitempty"`
	ExpiresAt       *time.Time  `json:"expires_at,omitempty"`
	Note            string      `json:"note,omitempty"`
	// Author names who made this change, for the history; optional.
	Author string `json:"author,omitempty"`
}

func NewID() string { return rand.Text() }

func ValidID(id string) bool { return idPattern.MatchString(id) }

func NormalizeSpace(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return DefaultSpace, nil
	}
	if value == "*" {
		return "", fmt.Errorf("%w: space %q is reserved", ErrInvalid, value)
	}
	if !spacePattern.MatchString(value) {
		return "", fmt.Errorf("%w: space must match %s", ErrInvalid, spacePattern)
	}
	return value, nil
}

// NormalizeFilterSpaces cleans a space filter. Empty or "*" selects every
// space.
func NormalizeFilterSpaces(values []string) ([]string, error) {
	out := []string{}
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if v == "*" {
			return nil, nil
		}
		if !spacePattern.MatchString(v) {
			return nil, fmt.Errorf("%w: space must match %s", ErrInvalid, spacePattern)
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

// NormalizeAuthor validates the optional, caller-supplied author name.
func NormalizeAuthor(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > MaxAuthorBytes || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%w: author must be a single line of at most %d bytes", ErrInvalid, MaxAuthorBytes)
	}
	return value, nil
}

func NormalizeKind(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return KindNote, nil
	}
	if !slices.Contains(Kinds, value) {
		return "", fmt.Errorf("%w: kind must be one of %s", ErrInvalid, strings.Join(Kinds, ", "))
	}
	return value, nil
}

// Normalize validates caller input. Evidence is only shape-checked here; the
// manager resolves it against the repository.
func (in Input) Normalize() (Input, error) {
	var err error
	if in.Space, err = NormalizeSpace(in.Space); err != nil {
		return in, err
	}
	if in.Kind, err = NormalizeKind(in.Kind); err != nil {
		return in, err
	}
	if in.Author, err = NormalizeAuthor(in.Author); err != nil {
		return in, err
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Content = strings.TrimSpace(in.Content)
	if in.Title == "" || len(in.Title) > MaxTitleBytes || strings.ContainsAny(in.Title, "\r\n") {
		return in, fmt.Errorf("%w: a single-line title of at most %d bytes is required", ErrInvalid, MaxTitleBytes)
	}
	if in.Content == "" || len(in.Content) > MaxContentBytes || !utf8.ValidString(in.Content) {
		return in, fmt.Errorf("%w: content is required and must be UTF-8 Markdown of at most %d bytes", ErrInvalid, MaxContentBytes)
	}
	if LooksSecret(in.Title + "\n" + in.Content) {
		return in, ErrSecret
	}
	tags := make([]string, 0, len(in.Tags))
	for _, tag := range in.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		if len(tag) > MaxTagBytes || strings.ContainsAny(tag, "\r\n,") {
			return in, fmt.Errorf("%w: tags are single words of at most %d bytes", ErrInvalid, MaxTagBytes)
		}
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	if len(tags) > MaxTags {
		return in, fmt.Errorf("%w: at most %d tags", ErrInvalid, MaxTags)
	}
	in.Tags = tags
	if len(in.Evidence) > MaxEvidence {
		return in, fmt.Errorf("%w: at most %d evidence entries", ErrInvalid, MaxEvidence)
	}
	evidence := make([]Evidence, 0, len(in.Evidence))
	for _, e := range in.Evidence {
		e.Repo, e.Path, e.Symbol = strings.TrimSpace(e.Repo), strings.TrimSpace(e.Path), strings.TrimSpace(e.Symbol)
		if e.Repo == "" || (e.Path == "" && e.Symbol == "") || len(e.Path) > 1024 || len(e.Symbol) > 256 {
			return in, fmt.Errorf("%w: evidence needs repo and a path or symbol", ErrInvalid)
		}
		if e.StartLine < 0 || e.EndLine < 0 || (e.EndLine > 0 && e.StartLine == 0) {
			return in, fmt.Errorf("%w: evidence line range must start at 1", ErrInvalid)
		}
		if e.StartLine > 0 && e.EndLine == 0 {
			e.EndLine = e.StartLine
		}
		if e.EndLine < e.StartLine || e.EndLine-e.StartLine > 500 {
			return in, fmt.Errorf("%w: evidence line range must be ordered and at most 500 lines", ErrInvalid)
		}
		e.Commit, e.Hash, e.Status = "", "", ""
		evidence = append(evidence, e)
	}
	in.Evidence = evidence
	if !in.ExpiresAt.IsZero() {
		in.ExpiresAt = in.ExpiresAt.UTC()
	}
	return in, nil
}

// ContentHash identifies exact duplicates regardless of whitespace and case.
func ContentHash(title, content string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(title+"\n"+content), " "))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

func searchText(m *Memory) string {
	parts := slices.Clone(m.Tags)
	for _, e := range m.Evidence {
		parts = append(parts, e.Repo, e.Path, e.Symbol)
	}
	return strings.Join(parts, " ")
}

// EffectiveStatus reports Expired for a memory past its ExpiresAt.
func (m *Memory) EffectiveStatus(now time.Time) string {
	if !m.ExpiresAt.IsZero() && now.After(m.ExpiresAt) {
		return StatusExpired
	}
	return m.Status
}

// StatusFromEvidence derives the memory status from its evidence checks.
func StatusFromEvidence(evidence []Evidence) string {
	status := StatusActive
	for _, e := range evidence {
		switch e.Status {
		case EvidenceMissing:
			return StatusOrphaned
		case EvidenceChanged:
			status = StatusStale
		}
	}
	return status
}

func AddEvent(m *Memory, e Event) {
	e.At = e.At.UTC()
	if len(e.Actor) > MaxAuthorBytes {
		e.Actor = e.Actor[:MaxAuthorBytes]
	}
	if len(e.Note) > MaxNoteBytes {
		e.Note = e.Note[:MaxNoteBytes]
	}
	m.History = append(m.History, e)
	if len(m.History) > MaxHistory {
		// Keep the creation event: it is the one people look for.
		m.History = append(m.History[:1], m.History[len(m.History)-MaxHistory+1:]...)
	}
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{32,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|access[_-]?token)\s*[:=]\s*["']?[^\s"'<>{}$]{8,}`),
}

// LooksSecret reports whether text appears to carry a credential. Memories are
// shared and searchable by every agent, so a false positive is cheaper than a
// leaked key.
func LooksSecret(text string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
