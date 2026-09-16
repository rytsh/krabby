// Package transfer implements offline, versioned bw transfers. The serving
// process and commands share a lock; imports are prepared in private generations
// and published by replacing one small pointer file, never by editing live data.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/rakunlabs/bw"
	"golang.org/x/sys/unix"
)

const formatVersion = 1

var databases = []string{"state", "docs-vectors", "code-vectors"}
var fileRoots = []string{"repos", "docs", "sources", "apis", "merged"}

// Status is the small cursor exchanged between environments. Versions belong
// to individual databases; ID identifies the complete DB + filesystem snapshot.
type Status struct {
	FormatVersion int               `json:"format_version"`
	DatasetID     string            `json:"dataset_id"`
	ID            string            `json:"id"`
	Versions      map[string]uint64 `json:"versions"`
}

type Database struct {
	Mode   string `json:"mode"` // full or delta
	Digest string `json:"digest"`
}

type Manifest struct {
	Status
	BaseID           string              `json:"base_id,omitempty"`
	AppVersion       string              `json:"app_version"`
	SourceRoot       string              `json:"source_root"`
	SourceWorkingDir string              `json:"source_working_dir"`
	Databases        map[string]Database `json:"databases"`
	Files            map[string]string   `json:"files"` // archive entry -> sha256
}

type ActiveDataset struct {
	Directory string
	Manifest  Manifest
}

// Lock excludes serving, exporting and importing in one data_dir. Unlike a
// mkdir lock, the OS releases it after a crash. Badger's own locks additionally
// protect against older Krabby processes which do not know about this lock.
func Lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".krabby.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("data directory is in use; stop Krabby before transferring data: %w", err)
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

// Active resolves only a generation owned by the transfer subsystem.
func Active(dir string) (*ActiveDataset, error) {
	var pointer struct {
		ID string `json:"id"`
	}
	err := readJSON(filepath.Join(dir, ".replica", "current.json"), &pointer)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read active dataset: %w", err)
	}
	if _, err := uuid.Parse(pointer.ID); err != nil {
		return nil, fmt.Errorf("invalid active dataset id")
	}
	root, err := filepath.Abs(filepath.Join(dir, ".replica", pointer.ID))
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := readJSON(filepath.Join(root, "manifest.json"), &manifest); err != nil {
		return nil, err
	}
	if manifest.ID != pointer.ID || manifest.FormatVersion != formatVersion {
		return nil, fmt.Errorf("invalid active manifest")
	}
	return &ActiveDataset{Directory: root, Manifest: manifest}, nil
}

func ReadStatus(path string) (Status, error) {
	var s Status
	err := readJSON(path, &s)
	if err == nil {
		err = validStatus(s)
	}
	return s, err
}

func validStatus(s Status) error {
	if s.FormatVersion != formatVersion {
		return fmt.Errorf("unsupported transfer format %d", s.FormatVersion)
	}
	if _, err := uuid.Parse(s.ID); err != nil {
		return fmt.Errorf("invalid snapshot id")
	}
	if _, err := uuid.Parse(s.DatasetID); err != nil {
		return fmt.Errorf("invalid dataset id")
	}
	return nil
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}

func writeJSON(path string, v any) error {
	return atomicFile(path, func(w io.Writer) error { return json.NewEncoder(w).Encode(v) })
}

func atomicFile(path string, write func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".transfer-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := write(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// digest is a streaming checksum of the live bw keyspace, including FTS and
// HNSW state. It is independent of backup stream ordering and DB file layout.
// No application records are decoded or compared.
func digest(ctx context.Context, db *bw.DB) (string, error) {
	h := sha256.New()
	err := db.Badger().View(func(tx *badger.Txn) error {
		it := tx.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			i := it.Item()
			for _, n := range []uint64{uint64(i.KeySize()), i.Version(), i.ExpiresAt()} {
				_ = binary.Write(h, binary.BigEndian, n)
			}
			_, _ = h.Write(i.Key())
			_, _ = h.Write([]byte{i.UserMeta()})
			if err := i.Value(func(v []byte) error {
				_ = binary.Write(h, binary.BigEndian, uint64(len(v)))
				_, _ = h.Write(v)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

func loadBackup(db *bw.DB, path string, full bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if full {
		return db.Restore(f)
	}
	return db.ApplyBackup(f)
}
