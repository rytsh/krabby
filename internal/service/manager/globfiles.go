package manager

import (
	"context"

	"github.com/rytsh/krabby/internal/service/repofs"
)

// GlobRepoFiles finds files in a tracked repo's clone by path pattern.
//
// It exists because no other read tool could answer "where are the Makefiles":
// code search matches file contents, and the listing walks one directory per
// call, so locating a file by the shape of its path meant guessing directories
// one round trip at a time.
//
// The snapshot token is resolved through repoCloneDirAt and the token actually
// read is returned, including the transparent fallback when a replayed token
// has already been reaped — a client that pins a glob and then reads one of its
// paths must stay on the same commit for both calls.
func (m *Manager) GlobRepoFiles(ctx context.Context, repoID, snapshot, pattern string, limit int) (repofs.GlobPage, error) {
	dir, token, err := m.repoCloneDirAt(ctx, repoID, snapshot)
	if err != nil {
		return repofs.GlobPage{}, err
	}

	page, err := repofs.GlobFiles(dir, pattern, limit)
	if err != nil {
		return repofs.GlobPage{}, err
	}
	page.Snapshot = token

	return page, nil
}
