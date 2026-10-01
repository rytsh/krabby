package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
)

// docsExt is the extension of every generated markdown document. Slugs are
// stored without it and paths carry it, so the two conversions below are the
// only places the spelling appears.
const docsExt = ".md"

// docFileName returns the markdown file name for a document slug.
func docFileName(slug string) string { return slug + docsExt }

// docSlug returns the document slug for a markdown file name.
func docSlug(path string) string { return strings.TrimSuffix(path, docsExt) }

func (m *Manager) docsDirForRepo(repo *registry.Repo) (string, error) {
	dir, rel, err := m.repoDocsPath(repo.ID)
	if err != nil {
		return "", err
	}

	legacy := filepath.Join(repo.Path, "krabby-docs")
	if err := migrateLegacyDocs(legacy, dir); err != nil {
		return "", fmt.Errorf("move %s to %s: %w", legacy, rel, err)
	}

	return dir, nil
}

func (m *Manager) repoDocsPath(repoID string) (dir, rel string, err error) {
	if m.docsRootDir == "" {
		return "", "", fmt.Errorf("docs root directory not configured")
	}

	rel = filepath.Clean(filepath.FromSlash(repoID))
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.ToSlash(rel) != repoID {
		return "", "", fmt.Errorf("invalid repository id %q", repoID)
	}

	return filepath.Join(m.docsRootDir, rel), rel, nil
}

func (m *Manager) removeRepoDocs(repoID string) error {
	_, rel, err := m.repoDocsPath(repoID)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(m.docsRootDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}
	defer func() { _ = root.Close() }()

	return root.RemoveAll(rel)
}

// ReadRepoFile returns the contents of a source file inside a tracked repo's
// clone. Access is sandboxed to the clone directory. offset/maxBytes paginate
// large files; maxBytes<=0 uses the repofs default cap.
func (m *Manager) ReadRepoFile(ctx context.Context, repoID, relPath string, offset int64, maxBytes int) (*repofs.FileContent, error) {
	return m.ReadRepoFileAt(ctx, repoID, relPath, "", offset, maxBytes)
}

// ReadRepoFileAt reads from a specific immutable snapshot when snapshot is set.
func (m *Manager) ReadRepoFileAt(ctx context.Context, repoID, relPath, snapshot string, offset int64, maxBytes int) (*repofs.FileContent, error) {
	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return nil, err
	}

	result, err := repofs.ReadFile(dir, relPath, offset, maxBytes)
	if err != nil {
		return nil, err
	}
	result.Snapshot = token

	return result, nil
}

// BlameCommit is the per-commit metadata referenced by blame hunks. It is held
// once in BlameFileResult.Commits and keyed by commit sha so the same commit is
// never repeated across hunks.
type BlameCommit struct {
	Author  string `json:"author"`
	Email   string `json:"email,omitempty"`
	Time    int64  `json:"time,omitempty"` // author time, unix seconds
	Summary string `json:"summary,omitempty"`
}

// BlameHunk is a run of consecutive lines attributed to the same commit.
type BlameHunk struct {
	Commit    string   `json:"commit"`     // sha; look up details in BlameFileResult.Commits
	LineStart int      `json:"line_start"` // 1-based, inclusive
	LineEnd   int      `json:"line_end"`   // 1-based, inclusive
	Lines     []string `json:"lines"`      // source lines for LineStart..LineEnd
}

// BlameFileResult carries structured git blame for a repo file. Consecutive
// lines from the same commit are grouped into hunks, and commit metadata is
// deduplicated into Commits (keyed by sha) so nothing is repeated.
type BlameFileResult struct {
	Repo     string                  `json:"repo"`
	Path     string                  `json:"path"`
	Start    int                     `json:"start,omitempty"`
	End      int                     `json:"end,omitempty"`
	Snapshot string                  `json:"snapshot,omitempty"`
	Commits  map[string]*BlameCommit `json:"commits"`
	Hunks    []BlameHunk             `json:"hunks"`
}

// BlameRepoFile runs `git blame` on a source file inside a tracked repo's clone.
// start/end limit the output to a line range (start<=0 blames the whole file,
// end<=0 blames start..EOF). When snapshot is set the blame is produced against
// that immutable snapshot; otherwise the active clone is used. Consecutive lines
// sharing a commit are collapsed into hunks and commit metadata is deduplicated.
func (m *Manager) BlameRepoFile(ctx context.Context, repoID, relPath, snapshot string, start, end int) (*BlameFileResult, error) {
	cleaned, err := repofs.CleanPath(relPath)
	if err != nil {
		return nil, err
	}

	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return nil, err
	}

	blameLines, err := m.git.Blame(ctx, dir, cleaned, start, end)
	if err != nil {
		return nil, err
	}

	res := &BlameFileResult{
		Repo:     repoID,
		Path:     cleaned,
		Start:    start,
		End:      end,
		Snapshot: token,
		Commits:  make(map[string]*BlameCommit),
	}

	for _, bl := range blameLines {
		if _, ok := res.Commits[bl.Commit]; !ok {
			res.Commits[bl.Commit] = &BlameCommit{
				Author:  bl.Author,
				Email:   bl.Email,
				Time:    bl.Time,
				Summary: bl.Summary,
			}
		}

		// Extend the current hunk when this line continues the same commit and
		// is contiguous; otherwise start a new hunk.
		if n := len(res.Hunks); n > 0 &&
			res.Hunks[n-1].Commit == bl.Commit &&
			res.Hunks[n-1].LineEnd+1 == bl.Line {
			res.Hunks[n-1].LineEnd = bl.Line
			res.Hunks[n-1].Lines = append(res.Hunks[n-1].Lines, bl.Content)

			continue
		}

		res.Hunks = append(res.Hunks, BlameHunk{
			Commit:    bl.Commit,
			LineStart: bl.Line,
			LineEnd:   bl.Line,
			Lines:     []string{bl.Content},
		})
	}

	return res, nil
}

// ListRepoFiles lists files under subdir ("" = repo root) in a tracked repo's
// clone. When recursive is true it walks the whole subtree.
func (m *Manager) ListRepoFiles(ctx context.Context, repoID, subdir string, recursive bool) ([]repofs.Entry, error) {
	entries, _, err := m.ListRepoFilesAt(ctx, repoID, subdir, "", recursive)

	return entries, err
}

// ListRepoFilesAt lists a specific immutable snapshot when snapshot is set.
func (m *Manager) ListRepoFilesAt(ctx context.Context, repoID, subdir, snapshot string, recursive bool) ([]repofs.Entry, string, error) {
	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return nil, "", err
	}

	entries, err := repofs.ListFiles(dir, subdir, recursive)

	return entries, token, err
}

// ListRepoFilesPage returns one cursor-paged page of a repository listing.
func (m *Manager) ListRepoFilesPage(ctx context.Context, repoID, subdir string, recursive bool, cursor string, perPage int) (repofs.EntryPage, error) {
	return m.ListRepoFilesPageAt(ctx, repoID, subdir, "", recursive, cursor, perPage)
}

// ListRepoFilesPageAt lists a specific immutable snapshot when snapshot is set.
// Paging is by cursor: pass back the previous page's NextCursor to continue,
// which is what makes entries past the old MaxListEntries cap reachable.
func (m *Manager) ListRepoFilesPageAt(ctx context.Context, repoID, subdir, snapshot string, recursive bool, cursor string, perPage int) (repofs.EntryPage, error) {
	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return repofs.EntryPage{}, err
	}

	result, err := repofs.ListFilesCursor(dir, subdir, cursor, recursive, perPage)
	if err != nil {
		return repofs.EntryPage{}, err
	}
	result.Snapshot = token

	return result, nil
}
