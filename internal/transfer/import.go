package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/rytsh/krabby/internal/storage"
)

// Import verifies and applies an archive into a new generation. A failed or
// interrupted import leaves current.json and the entire active dataset intact.
// Callers must hold Lock; imports into an existing publisher are rejected.
func Import(ctx context.Context, dir, input, appVersion string) (Status, error) {
	active, err := Active(dir)
	if err != nil {
		return Status{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); err == nil {
		return Status{}, fmt.Errorf("destination contains publisher state; use a separate replica data_dir")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	replica := filepath.Join(dir, ".replica")
	if err := os.MkdirAll(replica, 0o700); err != nil {
		return Status{}, err
	}
	work, err := os.MkdirTemp(replica, ".import-")
	if err != nil {
		return Status{}, err
	}
	defer os.RemoveAll(work)
	f, err := os.Open(input)
	if err != nil {
		return Status{}, err
	}
	m, err := readArchive(ctx, f, work)
	closeErr := f.Close()
	if err != nil {
		return Status{}, err
	}
	if closeErr != nil {
		return Status{}, closeErr
	}
	if err := validateImport(m, active, appVersion); err != nil {
		return Status{}, err
	}
	if active != nil && active.Manifest.ID == m.ID {
		return m.Status, nil
	}
	for _, name := range databases {
		entry, ok := m.Databases[name]
		if !ok {
			continue
		}
		dest := filepath.Join(work, name)
		if entry.Mode == "delta" {
			if err := copyDatabase(ctx, filepath.Join(active.Directory, name), dest); err != nil {
				return Status{}, err
			}
		}
		db, err := storage.Open(dest)
		if err != nil {
			return Status{}, err
		}
		applyErr := loadBackup(db, filepath.Join(work, "databases", name), entry.Mode == "full")
		var got string
		if applyErr == nil {
			got, applyErr = digest(ctx, db)
		}
		closeErr := db.Close()
		if applyErr != nil {
			return Status{}, fmt.Errorf("apply %s: %w", name, applyErr)
		}
		if closeErr != nil {
			return Status{}, closeErr
		}
		if got != entry.Digest {
			return Status{}, fmt.Errorf("%s does not match publisher after apply; request a full export", name)
		}
	}
	for _, name := range fileRoots {
		src := filepath.Join(work, "files", name)
		if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return Status{}, err
		}
		if err := os.Rename(src, filepath.Join(work, name)); err != nil {
			return Status{}, err
		}
	}
	if err := os.RemoveAll(filepath.Join(work, "files")); err != nil {
		return Status{}, err
	}
	if err := os.RemoveAll(filepath.Join(work, "databases")); err != nil {
		return Status{}, err
	}
	// Materialized SSH keys are deliberately not copied; the reader never clones.
	if err := os.MkdirAll(filepath.Join(work, "keys"), 0o700); err != nil {
		return Status{}, err
	}
	if err := validateReader(work, m); err != nil {
		return Status{}, err
	}
	if err := writeJSON(filepath.Join(work, "manifest.json"), m); err != nil {
		return Status{}, err
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	final := filepath.Join(replica, m.ID)
	if _, err := os.Lstat(final); err == nil {
		// A crash may have published the generation directory but not its
		// pointer. Replace only an owned, matching, inactive generation; the
		// freshly verified work tree is complete and safe to activate.
		var previous Manifest
		if err := readJSON(filepath.Join(final, "manifest.json"), &previous); err != nil || !reflect.DeepEqual(previous, m) {
			return Status{}, fmt.Errorf("generation %s already exists with different contents", m.ID)
		}
		if err := os.RemoveAll(final); err != nil {
			return Status{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	if err := syncTreeDirs(work); err != nil {
		return Status{}, err
	}
	if err := os.Rename(work, final); err != nil {
		return Status{}, err
	}
	if err := syncDir(replica); err != nil {
		return Status{}, err
	}
	if err := writeJSON(filepath.Join(replica, "current.json"), struct {
		ID string `json:"id"`
	}{m.ID}); err != nil {
		return Status{}, err
	}
	return m.Status, nil
}

func validateImport(m Manifest, active *ActiveDataset, appVersion string) error {
	if err := validStatus(m.Status); err != nil {
		return err
	}
	if m.AppVersion != appVersion {
		return fmt.Errorf("publisher runs %s; use the same Krabby version (local %s)", m.AppVersion, appVersion)
	}
	if !filepath.IsAbs(m.SourceRoot) {
		return fmt.Errorf("publisher root must be absolute")
	}
	if m.SourceWorkingDir != "" && !filepath.IsAbs(m.SourceWorkingDir) {
		return fmt.Errorf("publisher working directory must be absolute")
	}
	if active != nil {
		if m.DatasetID != active.Manifest.DatasetID {
			return fmt.Errorf("archive belongs to another dataset")
		}
		if m.ID == active.Manifest.ID {
			return nil
		}
	}
	if m.BaseID != "" && (active == nil || active.Manifest.ID != m.BaseID) {
		return fmt.Errorf("delta requires base snapshot %s; import that base or request a full export", m.BaseID)
	}
	if _, ok := m.Databases["state"]; !ok {
		return fmt.Errorf("archive has no state database")
	}
	if len(m.Databases) != len(m.Versions) {
		return fmt.Errorf("invalid database cursor inventory")
	}
	for entry := range m.Files {
		if name, ok := strings.CutPrefix(entry, "databases/"); ok {
			if _, exists := m.Databases[name]; !exists {
				return fmt.Errorf("undeclared database stream %s", name)
			}
		}
	}
	for name, entry := range m.Databases {
		if !slices.Contains(databases, name) {
			return fmt.Errorf("unknown database %q", name)
		}
		if _, ok := m.Versions[name]; !ok {
			return fmt.Errorf("missing database version")
		}
		if _, ok := m.Files["databases/"+name]; !ok {
			return fmt.Errorf("missing database stream")
		}
		if entry.Mode != "full" && entry.Mode != "delta" {
			return fmt.Errorf("invalid database mode")
		}
		if entry.Mode == "delta" && (m.BaseID == "" || active == nil) {
			return fmt.Errorf("incremental stream has no base")
		}
	}
	return nil
}

// Closed Badger directories can be copied byte-for-byte. No hard links: an
// unsuccessful load or compaction must never alter the previous generation.
func copyDatabase(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected non-regular database file %s", p)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		return atomicFile(target, func(w io.Writer) error { _, err := io.Copy(w, &contextReader{ctx: ctx, r: in}); return err })
	})
}

func syncTreeDirs(dir string) error {
	var dirs []string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, p := range slices.Backward(dirs) {
		if err := syncDir(p); err != nil {
			return err
		}
	}
	return nil
}
