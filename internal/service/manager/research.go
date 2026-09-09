package manager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/strutil"
)

// DocumentRead preserves the paged file response while carrying the identity
// and evidence class needed to distinguish a snapshot from an upstream read.
type DocumentRead struct {
	*repofs.FileContent
	ScopeKey       string          `json:"scope_key"`
	SourceKind     string          `json:"source_kind"`
	CollectionType string          `json:"collection_type,omitempty"`
	URL            string          `json:"url,omitempty"`
	Evidence       rag.DocEvidence `json:"evidence"`
}

func (m *Manager) GetDocDetails(ctx context.Context, key, path string, offset int64, maxBytes int) (DocumentRead, error) {
	content, err := m.GetDoc(ctx, key, path, offset, maxBytes)
	if err != nil {
		return DocumentRead{}, err
	}
	docs := []rag.Doc{{Repo: key, Path: path}}
	m.enrichDocSources(ctx, docs)
	doc := docs[0]
	return DocumentRead{FileContent: content, ScopeKey: doc.ScopeKey, SourceKind: doc.SourceKind,
		CollectionType: doc.CollectionType, URL: doc.URL, Evidence: doc.Evidence}, nil
}

type OverviewSection struct {
	Title  string `json:"title"`
	Offset int64  `json:"offset" jsonschema:"byte offset to pass to get_doc for this section"`
}

// RepoOverview is a bounded orientation, not a new LLM-generated answer. It
// reads the existing synthesis and exposes a navigable outline of that artifact.
type RepoOverview struct {
	Repo              string            `json:"repo"`
	Available         bool              `json:"available"`
	Path              string            `json:"path,omitempty"`
	Overview          string            `json:"overview,omitempty"`
	Truncated         bool              `json:"truncated,omitempty"`
	NextOffset        int64             `json:"next_offset,omitempty"`
	Sections          []OverviewSection `json:"sections,omitempty"`
	SectionsTruncated bool              `json:"sections_truncated,omitempty"`
	GeneratedAt       time.Time         `json:"generated_at,omitzero"`
	SummarizedFiles   int               `json:"summarized_files,omitempty" jsonschema:"files represented in the summary cache, not total repo coverage"`
	DocsStatus        string            `json:"docs_status,omitempty"`
	DocsCommit        string            `json:"docs_commit,omitempty" jsonschema:"commit recorded by the last successful docs stage; may be unknown"`
	CloneCommit       string            `json:"clone_commit,omitempty"`
	Stale             bool              `json:"stale,omitempty" jsonschema:"known mismatch between documentation and local clone commits"`
	EvidenceKind      string            `json:"evidence_kind"`
	Note              string            `json:"note"`
}

func (m *Manager) RepoOverview(ctx context.Context, id string) (RepoOverview, error) {
	out := RepoOverview{Repo: id, EvidenceKind: "generated_summary"}
	repo, err := m.reg.Get(ctx, id)
	if err != nil {
		return out, err
	}
	if repo == nil {
		return out, fmt.Errorf("unknown repository %q; use list_repos for its exact id", id)
	}
	out.DocsStatus = repo.Stages.Docs.Status
	out.DocsCommit, out.CloneCommit = repo.Stages.Docs.Commit, repo.LastCommit
	out.Stale = out.DocsCommit != "" && out.CloneCommit != "" && out.DocsCommit != out.CloneCommit
	out.Note = "Generated orientation is unavailable. Use search_code/read_file for implementation and repo_status to inspect the docs stage."
	if m.docsRootDir == "" {
		return out, nil
	}
	dir, err := m.repoDocsDir(ctx, id)
	if err != nil {
		return out, err
	}
	man, err := docgen.LoadManifest(dir)
	if err != nil {
		return out, err
	}
	if man == nil {
		return out, nil
	}
	var meta *docgen.DocMeta
	for _, candidate := range []string{docgen.DocName, "overview.md"} {
		for i := range man.Docs {
			if man.Docs[i].Path == candidate && man.Docs[i].SourcePath == "" {
				meta = &man.Docs[i]
				break
			}
		}
		if meta != nil {
			break
		}
	}
	if meta == nil {
		return out, nil
	}
	content, err := repofs.ReadFile(dir, meta.Path, 0, 128<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Available, out.Path = true, meta.Path
	out.GeneratedAt, out.SummarizedFiles = meta.Generated, len(man.Summaries)
	cut := min(len(content.Content), 4096)
	if cut < len(content.Content) {
		for cut > 0 && !utf8.RuneStart(content.Content[cut]) {
			cut--
		}
	}
	out.Overview = content.Content[:cut]
	out.Truncated = int64(cut) < content.TotalSize
	if out.Truncated {
		out.NextOffset = int64(cut)
	}
	outline := content.Content
	if content.Truncated {
		// A cut line may be a partial heading; do not advertise it as a
		// complete section title. The outline explicitly reports its limit.
		if end := strings.LastIndexByte(outline, '\n'); end >= 0 {
			outline = outline[:end+1]
		} else {
			outline = ""
		}
	}
	out.Sections, out.SectionsTruncated = overviewSections(outline, 30)
	out.SectionsTruncated = out.SectionsTruncated || content.Truncated
	out.Note = "Generated from selected, budget-limited source summaries; use as navigation, then verify behavior with search_code/read_file. Read a section with get_doc using this repo/path and its offset. Commit metadata compares the local clone, not the upstream branch."
	return out, nil
}

// overviewSections extracts Markdown ATX headings, excluding fenced code and
// indented code. Offsets refer to the original bytes, including CRLF/Unicode.
func overviewSections(text string, limit int) ([]OverviewSection, bool) {
	var sections []OverviewSection
	var fence byte
	var fenceLen, offset int
	for raw := range strings.SplitAfterSeq(text, "\n") {
		at := offset
		offset += len(raw)
		line := strings.TrimRight(raw, "\r\n")
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 {
			continue
		}
		if fence != 0 {
			n := leadingChars(trimmed, fence)
			if n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				fence = 0
			}
			continue
		}
		if len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := leadingChars(trimmed, trimmed[0])
			if n >= 3 && (trimmed[0] != '`' || !strings.Contains(trimmed[n:], "`")) {
				fence, fenceLen = trimmed[0], n
				continue
			}
		}
		level := leadingChars(trimmed, '#')
		if level == 0 || level > 6 || level < len(trimmed) && trimmed[level] != ' ' && trimmed[level] != '\t' {
			continue
		}
		title := strings.TrimSpace(trimmed[level:])
		base := strings.TrimRight(title, "#")
		if len(base) < len(title) && (strings.HasSuffix(base, " ") || strings.HasSuffix(base, "\t")) {
			title = strings.TrimSpace(base)
		}
		if title == "" {
			continue
		}
		if len(sections) == limit {
			return sections, true
		}
		sections = append(sections, OverviewSection{Title: strutil.Truncate(title, 160), Offset: int64(at)})
	}
	return sections, false
}

func leadingChars(s string, char byte) int {
	n := 0
	for n < len(s) && s[n] == char {
		n++
	}
	return n
}
