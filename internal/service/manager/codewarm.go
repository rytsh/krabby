package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"golang.org/x/sync/semaphore"

	"github.com/rytsh/krabby/internal/service/registry"
)

// WarmCodeSearch creates missing bw FTS indexes for repositories that were
// tracked before normal code search was introduced. Existing indexes are left
// untouched; regular refreshes keep them current afterwards.
//
// It is safe to run in the background: repos whose index is still missing are
// first marked pending, so a concurrent SearchCodeText for such a repo warms it
// on demand (ensureCodeIndex) and never returns partial results. Per-repo
// locking makes the background pass and an on-demand warm cooperate instead of
// double-indexing.
func (m *Manager) WarmCodeSearch(ctx context.Context) error {
	if m.codeText == nil {
		return nil
	}

	repos, err := m.reg.List(ctx)
	if err != nil {
		return err
	}

	// Mark every repo with a missing index as pending up front, so a search
	// that races this pass knows to warm on demand rather than read an empty or
	// half-filled index.
	pending := make([]*registry.Repo, 0, len(repos))
	for _, repo := range repos {
		if repo.Path == "" || !fileExists(filepath.Join(repo.Path, ".git")) {
			continue
		}

		hasIndex, err := m.codeText.HasRepo(ctx, repo.ID)
		if err != nil {
			slog.Error("check code index", "repo", repo.ID, "error", err)
			continue
		}
		if hasIndex {
			continue
		}

		m.markCodeWarmPending(repo.ID)
		pending = append(pending, repo)
	}

	var errs []error
	for _, repo := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.ensureCodeIndex(ctx, repo.ID, repo.Path); err != nil {
			errs = append(errs, fmt.Errorf("warm code index for %s: %w", repo.ID, err))
		}
	}

	return errors.Join(errs...)
}

// markCodeWarmPending records that repo's normal code index still needs
// building, creating its per-repo warm lock if absent.
func (m *Manager) markCodeWarmPending(repoID string) {
	m.codeWarmMu.Lock()
	defer m.codeWarmMu.Unlock()
	if _, ok := m.codeWarm[repoID]; !ok {
		m.codeWarm[repoID] = semaphore.NewWeighted(1)
	}
}

// codeWarmLock returns the per-repo warm lock and whether repo is pending. When
// not pending, the index is already built (or was never scheduled) and callers
// need not warm it.
func (m *Manager) codeWarmLock(repoID string) (*semaphore.Weighted, bool) {
	m.codeWarmMu.Lock()
	defer m.codeWarmMu.Unlock()
	lk, ok := m.codeWarm[repoID]
	return lk, ok
}

// clearCodeWarmPending drops repo from the pending set once its index exists.
func (m *Manager) clearCodeWarmPending(repoID string) {
	m.codeWarmMu.Lock()
	defer m.codeWarmMu.Unlock()
	delete(m.codeWarm, repoID)
}

// ensureCodeIndex builds the normal (full-text) code index for repo if it is
// still pending, serialized per repo so the background warm pass and an
// on-demand warm triggered by a search never index the same repo twice. A
// no-op once the index exists.
func (m *Manager) ensureCodeIndex(ctx context.Context, repoID, clonePath string) error {
	if m.codeText == nil {
		return nil
	}

	lk, pending := m.codeWarmLock(repoID)
	if !pending {
		return nil
	}

	// A search waiting for a background build must stop when its request is
	// cancelled, without cancelling the build that owns the permit.
	if err := lk.Acquire(ctx, 1); err != nil {
		return err
	}
	defer lk.Release(1)

	// Re-check under the lock: another caller may have finished warming while we
	// waited, in which case the repo is no longer pending.
	if _, still := m.codeWarmLock(repoID); !still {
		return nil
	}

	if clonePath == "" || !fileExists(filepath.Join(clonePath, ".git")) {
		m.clearCodeWarmPending(repoID)
		return nil
	}

	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.codeRag == nil {
		return nil
	}

	// The warm pass runs outside a build, so the record is fetched here rather
	// than threaded through: its overrides decide which files are indexed, and
	// warming with the install-wide selection would produce an index that
	// disagrees with every later refresh of the same repo.
	repo, err := m.reg.Get(ctx, repoID)
	if err != nil {
		return err
	}

	if err := d.codeRag.IndexText(ctx, repoID, clonePath, repoFilters(repo)); err != nil {
		return err
	}

	m.clearCodeWarmPending(repoID)

	// A warm pass grows the corpus like any other index run, so the tuning
	// derived from it follows. Best-effort: it only ever costs query speed.
	if err := m.codeText.RefreshStats(ctx); err != nil {
		slog.Warn("refresh code search stats", "repo", repoID, "error", err)
	}

	return nil
}
