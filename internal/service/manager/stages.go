package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/registry"
)

// Stage execution is shared by selective generation and background refresh.
// Callers retain scheduling, dependency and error policy: selective runs return
// failures, while refresh records them without failing a published graph.
type codeIndexOptions struct {
	changed        []string
	incremental    bool
	reportProgress bool
	refreshStats   bool
}

func (m *Manager) runCodeIndexStage(ctx context.Context, repo *registry.Repo, d *docsBundle, opts codeIndexOptions) error {
	var onProgress progress.Func
	if opts.reportProgress {
		onProgress = m.progressReporter(repo.ID, registry.StageCodeIndex)
		defer m.clearProgress(repo.ID, registry.StageCodeIndex)
	}

	err := m.runStage(ctx, repo, registry.StageCodeIndex, func() error {
		if d.codeRag == nil {
			return fmt.Errorf("code index is not configured")
		}
		if opts.incremental {
			return d.codeRag.IndexChangedProgress(ctx, repo.ID, repo.Path, opts.changed, repoFilters(repo), onProgress)
		}
		return d.codeRag.IndexProgress(ctx, repo.ID, repo.Path, repoFilters(repo), onProgress)
	})

	// Corpus statistics are only query tuning; failure must not fail indexing.
	if opts.refreshStats && m.codeText != nil {
		if statsErr := m.codeText.RefreshStats(ctx); statsErr != nil {
			slog.Warn("refresh code search stats", "repo", repo.ID, "error", statsErr)
		}
	}
	return err
}

type docsStageOptions struct {
	force          bool
	reportProgress bool
}

func (m *Manager) runDocsStage(ctx context.Context, repo *registry.Repo, d *docsBundle, dir string, dirErr error, opts docsStageOptions) (*docgen.Manifest, error) {
	genCtx := ctx
	if opts.reportProgress {
		genCtx = progress.With(ctx, m.progressReporter(repo.ID, registry.StageDocs))
		defer m.clearProgress(repo.ID, registry.StageDocs)
	}

	var manifest *docgen.Manifest
	err := m.runStage(ctx, repo, registry.StageDocs, func() error {
		if d.gen == nil {
			return fmt.Errorf("docs generation disabled: enable docs and configure the LLM in settings")
		}
		if dirErr != nil {
			return dirErr
		}
		var err error
		manifest, err = d.gen.Generate(genCtx, repo.ID, repo.Path, dir, repoDocsOverride(repo), opts.force)
		return err
	})
	return manifest, err
}

func (m *Manager) runDocsIndexStage(ctx context.Context, repo *registry.Repo, d *docsBundle, dir string, dirErr error) error {
	return m.runStage(ctx, repo, registry.StageDocsIndex, func() error {
		if d.rag == nil && m.docsText == nil {
			return fmt.Errorf("docs search index is not configured")
		}
		if dirErr != nil {
			return dirErr
		}
		return m.indexDocs(ctx, d, repo.ID, dir)
	})
}
