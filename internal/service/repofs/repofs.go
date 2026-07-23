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

// Dir excluded from listings; graphify output and VCS metadata are noise.
var skipDirs = map[string]bool{
	".git":         true,
	"graphify-out": true,
}

// FileContent is the result of reading a file, with pagination metadata.
type FileContent struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`      // bytes returned
	TotalSize int64  `json:"total_size"` // full file size on disk
	Truncated bool   `json:"truncated"`  // true when TotalSize > bytes returned
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
	Entries []Entry `json:"entries"`
	Page    int     `json:"page"`
	PerPage int     `json:"per_page"`
	HasMore bool    `json:"has_more"`
	Capped  bool    `json:"capped,omitempty"`
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
