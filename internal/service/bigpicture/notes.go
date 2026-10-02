package bigpicture

import (
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
)

// Per-source research notes are cached per workspace instance, keyed by a hash
// of everything that produced them, so an incremental run only asks the model
// about sources whose collected content changed.

var noteKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

const maxNoteBytes = 1 << 20

func notePath(p *Picture, key string) string {
	return path.Join(p.StorageID, "notes", key+".md")
}

// Note returns a cached note, if present.
func (s *Store) Note(p *Picture, key string) (string, bool) {
	if s == nil || p == nil || !storagePattern.MatchString(p.StorageID) || !noteKeyPattern.MatchString(key) {
		return "", false
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return "", false
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(notePath(p, key))
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxNoteBytes))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// SaveNote caches a note. Failures only cost a later model call, so they are
// returned for logging but never fail research.
func (s *Store) SaveNote(p *Picture, key, text string) error {
	if s == nil || p == nil || !storagePattern.MatchString(p.StorageID) || !noteKeyPattern.MatchString(key) || len(text) > maxNoteBytes {
		return ErrInvalid
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(path.Join(p.StorageID, "notes"), 0o700); err != nil {
		return err
	}
	name := notePath(p, key)
	tmp := name + ".tmp"
	if err := root.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

// PruneNotes removes cached notes not used by the latest run.
func (s *Store) PruneNotes(p *Picture, keep map[string]bool) {
	if s == nil || p == nil || !storagePattern.MatchString(p.StorageID) {
		return
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), path.Join(p.StorageID, "notes"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if key, ok := strings.CutSuffix(entry.Name(), ".md"); ok && keep[key] {
			continue
		}
		_ = root.Remove(path.Join(p.StorageID, "notes", entry.Name()))
	}
}
