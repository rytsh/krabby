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
	"slices"
	"strings"
)

// Limits keep responses bounded so a single call cannot exhaust memory or the
// caller's token budget.
const (
	// MaxFileBytes caps how much of a file is read in one call.
	MaxFileBytes = 512 * 1024
	// MaxListEntries caps how many entries a whole-listing call returns. It
	// binds ListFiles only; the cursor-paged listing needs no such cap because
	// every entry past it is reachable by paging.
	MaxListEntries = 2000
	// DefaultListPerPage is the page size used when a caller names none.
	DefaultListPerPage = 100
	// MaxListPerPage is the hard ceiling on one listing page.
	MaxListPerPage = 200
)

// Dir excluded from listings; graphify output, VCS metadata and vendored
// third-party trees are noise when browsing a repository.
var skipDirs = map[string]bool{
	".git":         true,
	"graphify-out": true,
	"vendor":       true,
}

// Dir excluded from a glob, which is narrower than the listing set on purpose.
// A glob is a question the caller wrote: "vendor/**" can only mean vendored
// code, and answering it with an empty page would be a wrong answer, not a
// tidier one. What stays pruned is what is not repository content at all -
// git's object store and krabby's own graphify output, both of which would
// otherwise flood a "**/*.json" with artefacts the caller never committed.
var globSkipDirs = map[string]bool{
	".git":         true,
	"graphify-out": true,
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

// EntryPage is one bounded page of a directory listing.
//
// Paging is by cursor, not page number: the previous contract rebuilt the whole
// sorted listing on every request and capped it at MaxListEntries, so each page
// cost a full walk and nothing past the cap could be reached at all — its
// "capped" flag was the API reporting that it had lost data.
type EntryPage struct {
	Entries []Entry `json:"entries"`
	PerPage int     `json:"per_page"`
	// NextCursor is the opaque token for the following page, empty at the end.
	NextCursor string `json:"next_cursor,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"` // pass back on continuation pages
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
	defer func() { _ = root.Close() }()

	f, err := root.Open(cleaned)
	if err != nil {
		return nil, fmt.Errorf("open %s; %w", cleaned, err)
	}
	defer func() { _ = f.Close() }()

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
	defer func() { _ = root.Close() }()

	var entries []Entry

	if recursive {
		entries, err = listRecursive(root, cleaned)
	} else {
		entries, err = listShallow(root, cleaned)
	}

	if err != nil {
		return nil, err
	}

	slices.SortFunc(entries, compareEntries)

	if len(entries) > MaxListEntries {
		entries = entries[:MaxListEntries]
	}

	return entries, nil
}

// Listings are ordered directories before files, then by path. ListFiles has
// ordered them this way since it existed and the tree views built on it depend
// on it, so the cursor pager reproduces that order exactly instead of inventing
// one.
func compareEntries(a, b Entry) int {
	if a.IsDir != b.IsDir {
		if a.IsDir {
			return -1
		}

		return 1
	}

	return strings.Compare(a.Path, b.Path)
}

// A cursor carries both halves of that sort key: which half of the listing
// (directories or files) the next page resumes in, and the last path emitted.
// The path alone would be ambiguous, because a recursive listing traverses the
// same tree twice and resuming in the wrong half either repeats every file or
// drops every one.
const (
	cursorDirs  = "d:"
	cursorFiles = "f:"
)

func encodeCursor(e Entry) string {
	if e.IsDir {
		return cursorDirs + e.Path
	}

	return cursorFiles + e.Path
}

// decodeCursor returns which half of the listing to resume in and the path to
// resume after. An unrecognised cursor is refused rather than guessed at: a
// misread cursor silently skips or repeats entries, which is the failure class
// this API exists to end.
func decodeCursor(cursor string) (bool, string, error) {
	switch {
	case cursor == "":
		return true, "", nil
	case strings.HasPrefix(cursor, cursorDirs):
		return true, strings.TrimPrefix(cursor, cursorDirs), nil
	case strings.HasPrefix(cursor, cursorFiles):
		return false, strings.TrimPrefix(cursor, cursorFiles), nil
	}

	return false, "", fmt.Errorf("invalid cursor %q; pass back next_cursor from the previous page verbatim, or omit it to start from the beginning", cursor)
}

// ListFilesCursor returns one page of the listing under subdir, resuming after
// cursor (empty starts at the beginning). NextCursor is empty once the listing
// is exhausted, and every entry is reachable — unlike the page-number contract
// it replaced, which could not return anything past MaxListEntries.
func ListFilesCursor(rootDir, subdir, cursor string, recursive bool, perPage int) (EntryPage, error) {
	cleaned, err := clean(subdir)
	if err != nil {
		return EntryPage{}, err
	}

	inDirs, after, err := decodeCursor(cursor)
	if err != nil {
		return EntryPage{}, err
	}

	if perPage <= 0 {
		perPage = DefaultListPerPage
	}

	if perPage > MaxListPerPage {
		perPage = MaxListPerPage
	}

	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return EntryPage{}, fmt.Errorf("open repo root; %w", err)
	}
	defer func() { _ = root.Close() }()

	// One entry past the page decides NextCursor, so a listing that ends
	// exactly on a page boundary ends there instead of handing out a cursor
	// that resolves to an empty page.
	want := perPage + 1

	var entries []Entry

	if recursive {
		entries, err = listCursorRecursive(root, cleaned, inDirs, after, want)
	} else {
		entries, err = listCursorShallow(root, cleaned, inDirs, after, want)
	}

	if err != nil {
		return EntryPage{}, err
	}

	page := EntryPage{Entries: entries, PerPage: perPage}
	if len(entries) > perPage {
		page.Entries = entries[:perPage]
		page.NextCursor = encodeCursor(entries[perPage-1])
	}

	return page, nil
}

// listCursorShallow pages one directory. There is no walk to prune here, so the
// cursor is applied as what it is: a position in the sorted listing.
func listCursorShallow(root *os.Root, dir string, inDirs bool, after string, want int) ([]Entry, error) {
	entries, err := listShallow(root, dir)
	if err != nil {
		return nil, err
	}

	slices.SortFunc(entries, compareEntries)

	// Comparing against the cursor's own sort position is what makes a cursor
	// in the file half exclude the entire directory half.
	cut := Entry{Path: after, IsDir: inDirs}

	page := make([]Entry, 0, want)

	for _, e := range entries {
		if compareEntries(cut, e) >= 0 {
			continue
		}

		page = append(page, e)

		if len(page) == want {
			break
		}
	}

	return page, nil
}

// listCursorRecursive walks the subtree in listing order. Directories come
// before files, so it runs the directory half first and continues into the file
// half only once the directory half is exhausted within this page.
func listCursorRecursive(root *os.Root, dir string, inDirs bool, after string, want int) ([]Entry, error) {
	page := make([]Entry, 0, want)

	if inDirs {
		if err := walkHalf(root, dir, true, after, want, &page); err != nil {
			return nil, err
		}

		if len(page) >= want {
			return page, nil
		}

		after = ""
	}

	if err := walkHalf(root, dir, false, after, want, &page); err != nil {
		return nil, err
	}

	return page, nil
}

// walkStep is one ordered thing to do in a directory: emit an entry, or descend
// into a subtree. A subtree's key is its path plus "/", because that is where
// every path inside it sorts among the directory's other children: "a.txt"
// sorts before "a/x.go", so a plain depth-first walk emits them in the wrong
// order and the cursor's "greater than the last path" rule would then skip
// entries.
type walkStep struct {
	key     string
	rel     string
	entry   fs.DirEntry
	descend bool
}

// readDir is indirected through a variable so the cursor walk's subtree
// pruning is observable: a pruning bug cannot be seen in the returned page —
// the entries stay correct while every resumed page silently reads the whole
// tree again, which is the cost this pager exists to remove.
var readDir = fs.ReadDir

func walkHalf(root *os.Root, dir string, wantDirs bool, after string, want int, out *[]Entry) error {
	children, err := readDir(root.FS(), dir)
	if err != nil {
		return nil //nolint:nilerr // skip unreadable directories, as ListFiles does
	}

	steps := make([]walkStep, 0, 2*len(children))

	for _, c := range children {
		rel := c.Name()
		if dir != "." {
			rel = path.Join(dir, c.Name())
		}

		switch {
		case c.IsDir():
			if skipDirs[c.Name()] {
				continue
			}

			if wantDirs && rel > after {
				steps = append(steps, walkStep{key: rel, rel: rel, entry: c})
			}

			if subtreeAfter(rel, after) {
				steps = append(steps, walkStep{key: rel + "/", rel: rel, descend: true})
			}
		case c.Type().IsRegular():
			// Symlinks and devices are skipped rather than followed, matching
			// WalkFiles: a link out of the clone must not surface as a file in
			// it.
			if !wantDirs && rel > after {
				steps = append(steps, walkStep{key: rel, rel: rel, entry: c})
			}
		}
	}

	slices.SortFunc(steps, func(a, b walkStep) int { return strings.Compare(a.key, b.key) })

	for _, st := range steps {
		if len(*out) >= want {
			return nil
		}

		if st.descend {
			if err := walkHalf(root, st.rel, wantDirs, after, want, out); err != nil {
				return err
			}

			continue
		}

		e := Entry{Path: st.rel, IsDir: st.entry.IsDir()}
		if !e.IsDir {
			if info, ierr := st.entry.Info(); ierr == nil {
				e.Size = info.Size()
			}
		}

		*out = append(*out, e)
	}

	return nil
}

// subtreeAfter reports whether any path under dir can sort after the cursor.
// Every path in the subtree shares the prefix dir+"/", so a cursor that is
// greater than that prefix and does not lie inside it leaves nothing in the
// subtree to emit, and the subtree is never read. That pruning is what makes
// resuming cost the remainder of the walk instead of all of it.
func subtreeAfter(dir, after string) bool {
	if after == "" {
		return true
	}

	prefix := dir + "/"

	return after < prefix || strings.HasPrefix(after, prefix)
}

func listShallow(root *os.Root, dir string) ([]Entry, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s; %w", dir, err)
	}
	defer func() { _ = f.Close() }()

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
