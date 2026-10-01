package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/registry"
)

// MigrateDocs moves generated documentation persisted by older versions from
// each clone's krabby-docs directory into the external docs root. It performs
// filesystem moves/copies only and never invokes doc generation.
func (m *Manager) MigrateDocs(ctx context.Context) error {
	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, repo := range repos {
		if repo.Path == "" {
			continue
		}

		if _, err := m.docsDirForRepo(repo); err != nil {
			errs = append(errs, fmt.Errorf("migrate docs for %s: %w", repo.ID, err))
		}
	}

	return errors.Join(errs...)
}

// MigrateRepoIDs re-keys repositories tracked under legacy truncated ids (the
// last two URL path segments) to the full-path ids ParseRepoID now derives
// from the stored git URL (nested groups included). The clone and generated
// docs move to the new id's directories; vectors and text-search entries
// indexed under the old id are dropped and the index stages reset so the next
// refresh rebuilds them under the new id.
func (m *Manager) MigrateRepoIDs(ctx context.Context) error {
	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, repo := range repos {
		newID, perr := gitops.ParseRepoID(repo.URL)
		if perr != nil || newID == repo.ID {
			continue
		}

		if err := m.migrateRepoID(ctx, repo, newID); err != nil {
			errs = append(errs, fmt.Errorf("migrate repo %s to %s: %w", repo.ID, newID, err))
		}
	}

	return errors.Join(errs...)
}

func (m *Manager) migrateRepoID(ctx context.Context, repo *registry.Repo, newID string) error {
	oldID := repo.ID

	if existing, err := m.reg.Get(ctx, newID); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("target id already tracked")
	}

	// Move the clone when it lives inside the managed repos directory.
	newPath := filepath.Join(m.reposDir, filepath.FromSlash(newID))
	oldSnapshotRoot := m.snapshotRoot(oldID)
	newSnapshotRoot := m.snapshotRoot(newID)
	switch {
	case repo.Path == "":
		repo.Path = newPath
	case pathWithin(repo.Path, oldSnapshotRoot):
		rel, err := filepath.Rel(oldSnapshotRoot, repo.Path)
		if err != nil {
			return fmt.Errorf("resolve snapshot path; %w", err)
		}
		if err := moveDir(oldSnapshotRoot, newSnapshotRoot); err != nil {
			return fmt.Errorf("move snapshots; %w", err)
		}
		repo.Path = filepath.Join(newSnapshotRoot, rel)
	case repo.Path != newPath && pathWithin(repo.Path, m.reposDir):
		if err := moveDir(repo.Path, newPath); err != nil {
			return fmt.Errorf("move clone; %w", err)
		}

		repo.Path = newPath
	}

	// Move the generated docs to the new id's directory.
	if m.docsRootDir != "" {
		oldDocs, _, oerr := m.repoDocsPath(oldID)
		newDocs, _, nerr := m.repoDocsPath(newID)
		if oerr == nil && nerr == nil {
			if err := moveDir(oldDocs, newDocs); err != nil {
				return fmt.Errorf("move generated docs; %w", err)
			}
		}
	}

	// Indexes key their entries by repo id: drop everything stored under the
	// old id and reset the index stages so the next refresh (or the startup
	// warm-up for text search) rebuilds them under the new id.
	d, release := m.acquireDocs()
	if d.rag != nil {
		if err := d.rag.DeleteRepo(ctx, oldID); err != nil {
			slog.Warn("drop docs vectors for legacy id", "repo", oldID, "error", err)
		}
	}
	if m.docsText != nil {
		if err := m.docsText.DeleteRepo(ctx, oldID); err != nil {
			slog.Warn("drop docs text entries for legacy id", "repo", oldID, "error", err)
		}
	}
	if d.codeRag != nil {
		if err := d.codeRag.DeleteRepo(ctx, oldID); err != nil {
			slog.Warn("drop code vectors for legacy id", "repo", oldID, "error", err)
		}
	}
	release()

	if m.codeText != nil {
		if err := m.codeText.DeleteRepo(ctx, oldID); err != nil {
			slog.Warn("drop text search entries for legacy id", "repo", oldID, "error", err)
		}
	}

	repo.Stages.DocsIndex = registry.StageState{}
	repo.Stages.CodeIndex = registry.StageState{}

	repo.ID = newID
	if err := m.reg.Upsert(ctx, repo); err != nil {
		return err
	}

	if err := m.reg.Delete(ctx, oldID); err != nil {
		return err
	}

	slog.Info("migrated repo to full-path id", "from", oldID, "to", newID)

	return nil
}

// moveDir renames src onto dst (creating dst's parent). A missing src is a
// no-op; an existing dst is an error so migrations never clobber data.
func moveDir(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}

	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination %s already exists", dst)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	return os.Rename(src, dst)
}

func migrateLegacyDocs(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}

	info, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("legacy docs path is not a directory")
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(src, dst); err == nil {
			slog.Info("migrated generated docs", "from", src, "to", dst)

			return nil
		}
	} else if err != nil {
		return err
	}

	if err := mergeLegacyDocs(src, dst); err != nil {
		return err
	}

	slog.Info("migrated generated docs", "from", src, "to", dst)

	return nil
}

// mergeLegacyDocs handles an existing destination and cross-device migrations.
// Existing destination files win; unique legacy files are retained.
func mergeLegacyDocs(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := mergeLegacyDocs(srcPath, dstPath); err != nil {
				return err
			}

			continue
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported legacy docs entry %s", srcPath)
		}

		if _, err := os.Stat(dstPath); err == nil {
			if err := os.Remove(srcPath); err != nil {
				return err
			}

			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if err := os.Rename(srcPath, dstPath); err != nil {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
			if err := os.Remove(srcPath); err != nil {
				return err
			}
		}
	}

	return os.Remove(src)
}
