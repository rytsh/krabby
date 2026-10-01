package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/websource"
)

// ErrDocsDisabled is returned by doc/RAG methods when the subsystem is off.
var ErrDocsDisabled = errors.New("docs/rag subsystem is not enabled")

// ListDocs returns the generated doc metadata for a repo from its manifest.
func (m *Manager) ListDocs(ctx context.Context, repoID string) ([]docgen.DocMeta, error) {
	if m.docsRootDir == "" {
		return nil, ErrDocsDisabled
	}

	dir, err := m.repoDocsDir(ctx, repoID)
	if err != nil {
		return nil, err
	}

	man, err := docgen.LoadManifest(dir)
	if err != nil {
		return nil, err
	}

	// No manifest means documentation generation has never completed for this
	// repository, which is a different answer from "this repository has no
	// documents" and one the caller can act on — the stage may be pending,
	// skipped through skip_stages, or have failed.
	if man == nil {
		return nil, fmt.Errorf("no generated documentation for %s yet; check repo_status for the docs stage, or refresh_repo to build it", repoID)
	}

	if len(man.Docs) == 0 {
		return []docgen.DocMeta{}, nil
	}

	return man.Docs, nil
}

// GetDoc returns one generated markdown doc. Path is relative to that repo's
// external docs directory and access is sandboxed to it.
func (m *Manager) GetDoc(ctx context.Context, repoID, docPath string, offset int64, maxBytes int) (*repofs.FileContent, error) {
	if m.docsRootDir == "" && !strings.HasPrefix(repoID, websource.ScopePrefix) && !strings.HasPrefix(repoID, apicatalog.ScopePrefix) {
		return nil, ErrDocsDisabled
	}

	dir, err := m.repoDocsDir(ctx, repoID)
	if err != nil {
		return nil, err
	}

	return repofs.ReadFile(dir, docPath, offset, maxBytes)
}

// repoDocsDir resolves a docs key to its markdown directory: "web:<name>"
// keys map to the collection's synced content, "api:<name>" keys to the
// service's endpoint projections, repo ids to the repo's external docs
// directory (verifying the repo is tracked and cloned and migrating legacy
// in-clone docs when needed).
func (m *Manager) repoDocsDir(ctx context.Context, repoID string) (string, error) {
	if strings.HasPrefix(repoID, apicatalog.ScopePrefix) {
		name := apicatalog.ServiceName(repoID)
		if !apicatalog.ValidName(name) {
			return "", fmt.Errorf("invalid api scope %q", repoID)
		}
		if m.apisRootDir == "" || m.apiStore == nil {
			return "", ErrNoAPICatalog
		}
		svc, err := m.apiStore.GetService(ctx, name)
		if err != nil {
			return "", err
		}
		if svc == nil {
			return "", fmt.Errorf("api service %s not found", name)
		}

		return m.apisDir(name), nil
	}

	if strings.HasPrefix(repoID, websource.ScopePrefix) {
		name := websource.CollectionName(repoID)
		if !websource.ValidName(name) {
			return "", fmt.Errorf("invalid web source scope %q", repoID)
		}
		if m.sourcesRootDir == "" {
			return "", ErrNoWebSources
		}
		if m.webStore == nil {
			return "", ErrNoWebSources
		}
		col, err := m.webStore.GetCollection(ctx, name)
		if err != nil {
			return "", err
		}
		if col == nil {
			return "", fmt.Errorf("web source %s not found", name)
		}

		return m.sourcesDir(name), nil
	}

	repo, err := m.reg.Get(ctx, repoID)
	if err != nil {
		return "", err
	}
	if repo == nil {
		return "", fmt.Errorf("repo %s not found", repoID)
	}
	if repo.Path == "" || !fileExists(filepath.Join(repo.Path, ".git")) {
		return "", fmt.Errorf("repo %s not cloned yet (status: %s)", repoID, repo.Status)
	}

	return m.docsDirForRepo(repo)
}
