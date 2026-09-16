package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"

	"github.com/google/uuid"
	"github.com/rytsh/krabby/internal/storage"
)

// Export writes a self-contained archive and retains a full bw checkpoint for
// subsequent --since exports. Callers must hold Lock for the entire operation.
// Files are a full snapshot; bw alone computes database deltas.
func Export(ctx context.Context, dir, output, appVersion string, since *Status) (Status, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Status{}, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return Status{}, err
	}
	if rel, err := filepath.Rel(dir, output); err != nil || filepath.IsLocal(rel) {
		return Status{}, fmt.Errorf("export output must be outside data_dir")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		return Status{}, fmt.Errorf("output already exists or is inaccessible: %s", output)
	}
	if active, err := Active(dir); err != nil || active != nil {
		return Status{}, fmt.Errorf("export requires a publisher data directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "MANIFEST")); err != nil {
		return Status{}, fmt.Errorf("publisher state database: %w", err)
	}
	checkpoints := filepath.Join(dir, ".transfer", "checkpoints")
	if err := os.MkdirAll(checkpoints, 0o700); err != nil {
		return Status{}, err
	}
	var identity struct {
		ID string `json:"id"`
	}
	identityFile := filepath.Join(dir, ".transfer", "source.json")
	if err := readJSON(identityFile, &identity); errors.Is(err, os.ErrNotExist) {
		identity.ID = uuid.NewString()
		if err := writeJSON(identityFile, identity); err != nil {
			return Status{}, err
		}
	} else if err != nil {
		return Status{}, err
	}
	var base *Manifest
	if _, err := uuid.Parse(identity.ID); err != nil {
		return Status{}, fmt.Errorf("invalid publisher identity")
	}
	if since != nil {
		if err := validStatus(*since); err != nil {
			return Status{}, err
		}
		if since.DatasetID != identity.ID {
			return Status{}, fmt.Errorf("cursor belongs to another dataset")
		}
		var m Manifest
		err := readJSON(filepath.Join(checkpoints, since.ID, "manifest.json"), &m)
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("checkpoint no longer retained; exporting a full snapshot")
		} else if err != nil {
			return Status{}, err
		} else if !reflect.DeepEqual(m.Status, *since) {
			return Status{}, fmt.Errorf("cursor does not match publisher checkpoint")
		} else if m.AppVersion == appVersion {
			base = &m
		}
	}
	work, err := os.MkdirTemp(checkpoints, ".export-")
	if err != nil {
		return Status{}, err
	}
	defer os.RemoveAll(work)
	m := Manifest{
		Status:     Status{FormatVersion: formatVersion, DatasetID: identity.ID, ID: uuid.NewString(), Versions: map[string]uint64{}},
		AppVersion: appVersion, SourceRoot: dir, Databases: map[string]Database{}, Files: map[string]string{},
	}
	m.SourceWorkingDir, err = os.Getwd()
	if err != nil {
		return Status{}, err
	}
	if base != nil {
		m.BaseID = base.ID
	}
	streams := map[string]string{}
	for _, name := range databases {
		if err := ctx.Err(); err != nil {
			return Status{}, err
		}
		if _, err := os.Stat(filepath.Join(dir, name, "MANIFEST")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return Status{}, err
		}
		entry, version, stream, err := exportDatabase(ctx, filepath.Join(dir, name), name, work, checkpoints, base)
		if err != nil {
			return Status{}, fmt.Errorf("export %s: %w", name, err)
		}
		m.Databases[name], m.Versions[name], streams[name] = entry, version, stream
	}
	if err := atomicFile(output, func(w io.Writer) error { return writeArchive(ctx, w, dir, streams, &m) }); err != nil {
		return Status{}, err
	}
	if err := writeJSON(filepath.Join(work, "manifest.json"), m); err != nil {
		_ = os.Remove(output)
		return Status{}, err
	}
	if err := os.Rename(work, filepath.Join(checkpoints, m.ID)); err != nil {
		_ = os.Remove(output)
		return Status{}, err
	}
	if err := syncDir(checkpoints); err != nil {
		return Status{}, err
	}
	return m.Status, nil
}

func exportDatabase(ctx context.Context, dir, name, work, checkpoints string, base *Manifest) (Database, uint64, string, error) {
	db, err := storage.Open(dir)
	if err != nil {
		return Database{}, 0, "", err
	}
	defer db.Close()
	full := filepath.Join(work, name+".full")
	var version uint64
	err = atomicFile(full, func(w io.Writer) error {
		var err error
		version, err = db.Backup(w, 0, true)
		return err
	})
	if err != nil {
		return Database{}, 0, "", err
	}
	sum, err := digest(ctx, db)
	if err != nil {
		return Database{}, 0, "", err
	}
	entry := Database{Mode: "full", Digest: sum}
	if base == nil {
		return entry, version, full, nil
	}
	since, exists := base.Versions[name]
	if !exists || since > version {
		return entry, version, full, nil
	}
	delta := filepath.Join(work, name+".delta")
	err = atomicFile(delta, func(w io.Writer) error { _, err := db.Backup(w, since, true); return err })
	if err != nil {
		return entry, version, "", err
	}

	// Compaction can discard old delete markers, and a Wipe/schema migration
	// can invalidate an incremental history. Prove the delta against the saved
	// checkpoint using bw itself. If it cannot reproduce today's keyspace,
	// transparently send a full backup for this database instead.
	verifyDir := filepath.Join(work, name+".verify")
	verified, err := storage.Open(verifyDir)
	if err != nil {
		return entry, version, "", err
	}
	verifyErr := loadBackup(verified, filepath.Join(checkpoints, base.ID, name+".full"), true)
	if verifyErr == nil {
		verifyErr = loadBackup(verified, delta, false)
	}
	var got string
	if verifyErr == nil {
		got, verifyErr = digest(ctx, verified)
	}
	closeErr := verified.Close()
	if err := os.RemoveAll(verifyDir); err != nil {
		return entry, version, "", err
	}
	if closeErr != nil {
		return entry, version, "", closeErr
	}
	if err := ctx.Err(); err != nil {
		return entry, version, "", err
	}
	if verifyErr == nil && got == sum {
		entry.Mode = "delta"
		return entry, version, delta, nil
	}
	slog.Info("incremental history is incomplete; sending full database", "database", name)
	return entry, version, full, nil
}
