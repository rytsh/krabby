package manager

import (
	"context"
	"fmt"
	"strings"

	"github.com/rytsh/krabby/internal/service/gitops"
	"github.com/rytsh/krabby/internal/service/repofs"
)

// maxRefs caps one page of refs. Long-lived repositories accumulate thousands
// of tags, and a caller looking for "the previous release" needs the newest
// handful, not all of them.
const maxRefs = 200

// RepoRefsResult is the tag/branch listing of one repository.
type RepoRefsResult struct {
	Repo     string       `json:"repo"`
	Snapshot string       `json:"snapshot,omitempty"`
	Refs     []gitops.Ref `json:"refs"`
	Total    int          `json:"total"`
	HasMore  bool         `json:"has_more,omitempty"`
}

// RepoRefs lists the tags and branches of a tracked repository, newest first.
//
// kind filters to "tag" or "branch"; empty returns both. Only the tracked
// branch is ever fetched (clones are --single-branch), so the branch list is
// short by construction and tags are what callers actually navigate by.
func (m *Manager) RepoRefs(ctx context.Context, repoID, kind string, limit int) (*RepoRefsResult, error) {
	dir, token, err := m.repoCloneDirAt(ctx, repoID, "")
	if err != nil {
		return nil, err
	}

	refs, err := m.git.Refs(ctx, dir)
	if err != nil {
		return nil, err
	}

	if kind = strings.TrimSpace(strings.ToLower(kind)); kind != "" {
		kept := refs[:0]
		for _, r := range refs {
			if r.Type == kind {
				kept = append(kept, r)
			}
		}
		refs = kept
	}

	if limit <= 0 || limit > maxRefs {
		limit = maxRefs
	}

	out := &RepoRefsResult{Repo: repoID, Snapshot: token, Total: len(refs)}
	if len(refs) > limit {
		refs = refs[:limit]
		out.HasMore = true
	}
	out.Refs = refs

	return out, nil
}

// RepoLogResult is a page of commit history.
type RepoLogResult struct {
	Repo     string          `json:"repo"`
	Snapshot string          `json:"snapshot,omitempty"`
	From     string          `json:"from,omitempty"`
	To       string          `json:"to,omitempty"`
	Path     string          `json:"path,omitempty"`
	Commits  []gitops.Commit `json:"commits"`
	// HasMore reports that the page filled up, so an older page exists. The
	// exact remaining count is deliberately not computed: counting it means
	// walking the rest of the history, which is the cost the page limit exists
	// to avoid.
	HasMore bool `json:"has_more,omitempty"`
	Skip    int  `json:"skip,omitempty"`
}

// RepoLog reads commit metadata from a tracked repository's clone.
//
// Only the tracked branch is fetched, so both endpoints must be reachable from
// it: release tags are, a feature branch on the remote is not. That limit is
// reported as a plain "unknown revision" error from git rather than guessed at
// here.
func (m *Manager) RepoLog(ctx context.Context, repoID string, opts gitops.LogOptions) (*RepoLogResult, error) {
	if opts.Path != "" {
		cleaned, err := repofs.CleanPath(opts.Path)
		if err != nil {
			return nil, err
		}
		opts.Path = cleaned
	}

	dir, token, err := m.repoCloneDirAt(ctx, repoID, "")
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 || limit > gitops.MaxLogCommits {
		limit = gitops.MaxLogCommits
	}
	opts.Limit = limit

	commits, err := m.git.Log(ctx, dir, opts)
	if err != nil {
		return nil, err
	}

	return &RepoLogResult{
		Repo:     repoID,
		Snapshot: token,
		From:     opts.From,
		To:       opts.To,
		Path:     opts.Path,
		Commits:  commits,
		HasMore:  len(commits) == limit,
		Skip:     opts.Skip,
	}, nil
}

// RepoDiffResult is a change set between two revisions of one repository.
type RepoDiffResult struct {
	Repo     string `json:"repo"`
	Snapshot string `json:"snapshot,omitempty"`
	Path     string `json:"path,omitempty"`

	*gitops.DiffResult
}

// RepoDiff reports what changed between two revisions, or what a single commit
// changed when from is empty. patch controls whether the actual change text is
// included; the per-file status list always is.
func (m *Manager) RepoDiff(ctx context.Context, repoID, from, to, relPath string, patch bool) (*RepoDiffResult, error) {
	if strings.TrimSpace(to) == "" {
		return nil, fmt.Errorf("to revision is required")
	}

	if relPath != "" {
		cleaned, err := repofs.CleanPath(relPath)
		if err != nil {
			return nil, err
		}
		relPath = cleaned
	}

	dir, token, err := m.repoCloneDirAt(ctx, repoID, "")
	if err != nil {
		return nil, err
	}

	res, err := m.git.Diff(ctx, dir, from, to, relPath, patch)
	if err != nil {
		return nil, err
	}

	return &RepoDiffResult{Repo: repoID, Snapshot: token, Path: relPath, DiffResult: res}, nil
}
