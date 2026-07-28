// Package repofs provides sandboxed read-only access to files inside a tracked
// repository's clone directory. It exists so remote MCP clients (which have no
// filesystem access to the krabby host) can read source that the knowledge
// graph references by path.
//
// All access is confined to the repo root via os.Root, so path traversal
// ("../", absolute paths, symlinks escaping the root) is rejected by the OS.
package repofs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Limits keep responses bounded so a single call cannot exhaust memory or the
// caller's token budget.
const (
	// MaxFileBytes caps how much of a file is read in one call.
	MaxFileBytes = 512 * 1024
	// MaxListEntries caps how many entries a listing returns.
	MaxListEntries = 2000
)

// ErrTooLarge indicates the requested read exceeds MaxFileBytes.
var ErrTooLarge = errors.New("file too large")

// Dir excluded from listings; graphify output, VCS metadata and vendored
// third-party trees are noise.
var skipDirs = map[string]bool{
	".git":         true,
	"graphify-out": true,
	"vendor":       true,
}

// WalkFiles walks every regular file under rootDir, calling fn with the
// repo-relative slash path and its size. skipDir, when non-nil, is consulted
// for each directory (repo-relative path and base name); returning true prunes
// the whole subtree.
//
// It exists because ListFiles caps its result at MaxListEntries. That cap is
// right for a listing sent to a UI or an MCP client, and wrong for anything
// that has to see the whole repository: a caller that selects files to index or
// document would silently stop at the cap and quietly ignore everything past
// it, which is the kind of failure nobody notices until a large repository is
// half-documented.
//
// Symlinks and other non-regular entries are skipped rather than followed, so a
// link pointing outside the clone cannot pull foreign files into a walk.
func WalkFiles(rootDir string, skipDir func(rel, name string) bool, fn func(rel string, size int64) error) error {
	return filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == rootDir {
			return nil
		}

		rel, err := filepath.Rel(rootDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			// Only .git is pruned unconditionally: walking a repository's own
			// object store is never useful. Every other exclusion belongs to
			// the caller, whose rules are configurable — pruning here what
			// ListFiles happens to hide would make an explicit "index vendor/"
			// silently impossible.
			if d.Name() == ".git" || (skipDir != nil && skipDir(rel, d.Name())) {
				return fs.SkipDir
			}

			return nil
		}

		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		return fn(rel, info.Size())
	})
}

// DeployConfigFile reports whether a repo-relative path is a deployment or CI
// config file whose name follows a family convention rather than being fixed,
// so a plain name lookup cannot catch it: compose files carry the environment
// in the middle ("docker-compose.prod.yml"), Helm values likewise
// ("values-stage.yaml"), and GitHub workflows are named freely inside one
// well-known directory.
//
// It exists because these files are where image tags, service versions and
// deploy topology live — the same reason Dockerfile and go.mod are indexed —
// while indexing every .yaml instead would pull in the far larger body of
// generated manifests and vendored chart output.
//
// It lives here, in the package both the code indexer and the doc generator
// already depend on, so the two cannot drift on what counts as deploy config.
func DeployConfigFile(rel string) bool {
	rel = strings.ToLower(rel)

	if ext := path.Ext(rel); ext != ".yml" && ext != ".yaml" {
		return false
	}

	if strings.HasPrefix(rel, ".github/workflows/") || strings.Contains(rel, "/.github/workflows/") {
		return true
	}

	stem := strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	for _, family := range deployConfigStems {
		// An exact stem, or the family followed by a separator: the variant
		// suffix is what names the environment, which is the whole point of
		// indexing these ("docker-compose.prod.yml" vs ".stage.yml").
		if stem == family ||
			strings.HasPrefix(stem, family+".") ||
			strings.HasPrefix(stem, family+"-") {
			return true
		}
	}

	return false
}

// deployConfigStems are the YAML config families DeployConfigFile recognises,
// with or without an environment suffix.
var deployConfigStems = []string{
	"compose",
	"docker-compose",
	"docker-stack",
	"values",
	"chart",
	".gitlab-ci",
}

// FileContent is the result of reading a file, with pagination metadata.
type FileContent struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`              // bytes returned
	TotalSize int64  `json:"total_size"`         // full file size on disk
	Truncated bool   `json:"truncated"`          // true when TotalSize > bytes returned
	Snapshot  string `json:"snapshot,omitempty"` // pass back on continuation reads
}

// Entry is one item in a directory listing.
type Entry struct {
	Path  string `json:"path"` // repo-relative, slash-separated
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"` // bytes, files only
}

// EntryPage is a bounded page of a directory listing. Capped means the listing
// reached the repository-wide safety limit and later entries are unavailable.
type EntryPage struct {
	Entries  []Entry `json:"entries"`
	Page     int     `json:"page"`
	PerPage  int     `json:"per_page"`
	HasMore  bool    `json:"has_more"`
	Capped   bool    `json:"capped,omitempty"`
	Snapshot string  `json:"snapshot,omitempty"` // pass back on continuation pages
}

// clean normalises a user-supplied repo-relative path and rejects anything that
// would escape the root. os.Root also enforces this, but we reject early for a
// clearer error and to keep listings tidy.
func clean(rel string) (string, error) {
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	if rel == "" || rel == "." {
		return ".", nil
	}

	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes repository root", rel)
	}

	return cleaned, nil
}

// CleanPath normalises a user-supplied repo-relative path and rejects anything
// that would escape the repository root. Callers that hand the path to an
// external tool (e.g. `git blame`) instead of os.Root must run it through this
// first so path traversal cannot reach outside the clone. The returned path is
// slash-separated and never "." for a concrete file.
func CleanPath(rel string) (string, error) {
	cleaned, err := clean(rel)
	if err != nil {
		return "", err
	}
	if cleaned == "." {
		return "", fmt.Errorf("path is a directory, not a file")
	}

	return cleaned, nil
}

// ReadFile returns up to maxBytes of a file, starting at byte offset. maxBytes
// <= 0 uses MaxFileBytes; anything larger is capped at MaxFileBytes.
func ReadFile(rootDir, rel string, offset int64, maxBytes int) (*FileContent, error) {
	cleaned, err := clean(rel)
	if err != nil {
		return nil, err
	}

	if cleaned == "." {
		return nil, fmt.Errorf("path is a directory, not a file")
	}

	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("open repo root; %w", err)
	}
	defer root.Close()

	f, err := root.Open(cleaned)
	if err != nil {
		return nil, fmt.Errorf("open %s; %w", cleaned, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s; %w", cleaned, err)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a file", cleaned)
	}

	if maxBytes <= 0 || maxBytes > MaxFileBytes {
		maxBytes = MaxFileBytes
	}

	if offset < 0 {
		offset = 0
	}

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return nil, fmt.Errorf("seek %s; %w", cleaned, err)
		}
	}

	buf := make([]byte, maxBytes)

	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read %s; %w", cleaned, err)
	}

	truncated := offset+int64(n) < info.Size()

	return &FileContent{
		Path:      cleaned,
		Content:   string(buf[:n]),
		Bytes:     n,
		TotalSize: info.Size(),
		Truncated: truncated,
	}, nil
}

// ListFiles returns entries under subdir (repo-relative; "" = root). When
// recursive is true it walks the whole subtree, skipping .git and graphify-out.
func ListFiles(rootDir, subdir string, recursive bool) ([]Entry, error) {
	cleaned, err := clean(subdir)
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("open repo root; %w", err)
	}
	defer root.Close()

	var entries []Entry

	if recursive {
		entries, err = listRecursive(root, cleaned)
	} else {
		entries, err = listShallow(root, cleaned)
	}

	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // dirs first
		}

		return entries[i].Path < entries[j].Path
	})

	if len(entries) > MaxListEntries {
		entries = entries[:MaxListEntries]
	}

	return entries, nil
}

// ListFilesPage returns a bounded page from the safety-capped stable listing.
func ListFilesPage(rootDir, subdir string, recursive bool, page, perPage int) (EntryPage, error) {
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = 100
	}
	if perPage > 200 {
		perPage = 200
	}

	entries, err := ListFiles(rootDir, subdir, recursive)
	if err != nil {
		return EntryPage{}, err
	}

	offset := len(entries)
	if page-1 <= len(entries)/perPage {
		offset = (page - 1) * perPage
	}
	if offset > len(entries) {
		offset = len(entries)
	}
	end := offset + perPage
	if end > len(entries) {
		end = len(entries)
	}

	return EntryPage{
		Entries: entries[offset:end],
		Page:    page,
		PerPage: perPage,
		HasMore: end < len(entries),
		Capped:  len(entries) == MaxListEntries,
	}, nil
}

func listShallow(root *os.Root, dir string) ([]Entry, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s; %w", dir, err)
	}
	defer f.Close()

	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("read dir %s; %w", dir, err)
	}

	entries := make([]Entry, 0, len(names))

	for _, name := range names {
		if dir == "." && skipDirs[name] {
			continue
		}

		rel := name
		if dir != "." {
			rel = path.Join(dir, name)
		}

		info, err := root.Stat(rel)
		if err != nil {
			continue
		}

		e := Entry{Path: rel, IsDir: info.IsDir()}
		if !info.IsDir() {
			e.Size = info.Size()
		}

		entries = append(entries, e)
	}

	return entries, nil
}

func listRecursive(root *os.Root, dir string) ([]Entry, error) {
	var entries []Entry

	walkFn := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries
		}

		if p == "." {
			return nil
		}

		base := path.Base(p)
		if d.IsDir() && skipDirs[base] {
			return fs.SkipDir
		}

		e := Entry{Path: p, IsDir: d.IsDir()}
		if !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				e.Size = info.Size()
			}
		}

		entries = append(entries, e)

		if len(entries) >= MaxListEntries {
			return fs.SkipAll
		}

		return nil
	}

	if err := fs.WalkDir(root.FS(), dir, walkFn); err != nil {
		return nil, fmt.Errorf("walk %s; %w", dir, err)
	}

	return entries, nil
}
