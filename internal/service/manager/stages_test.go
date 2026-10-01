package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/registry"
)

type stageGeneratorFunc func(context.Context, string, string, string, config.DocsOverride, bool) (*docgen.Manifest, error)

func (f stageGeneratorFunc) Generate(ctx context.Context, repo, clone, dir string, over config.DocsOverride, force bool) (*docgen.Manifest, error) {
	return f(ctx, repo, clone, dir, over, force)
}

func TestSharedDocsStageOptionsAndFailureState(t *testing.T) {
	for _, report := range []bool{false, true} {
		t.Run(map[bool]string{false: "selective", true: "refresh"}[report], func(t *testing.T) {
			m, reg := newStageTestManager(t)
			m.activity = map[string]map[string]struct{}{}
			m.progress = map[string]map[string]Progress{}
			repo := &registry.Repo{ID: "owner/repo", Path: "clone", LastCommit: "abc123"}
			dir := t.TempDir()
			wantErr := errors.New("generation failed")
			manifest := &docgen.Manifest{ChangedDocs: true}
			d := &docsBundle{gen: stageGeneratorFunc(func(ctx context.Context, id, clone, gotDir string, _ config.DocsOverride, force bool) (*docgen.Manifest, error) {
				if id != repo.ID || clone != repo.Path || gotDir != dir || force != !report {
					t.Errorf("unexpected generation arguments: %s %s %s %t", id, clone, gotDir, force)
				}
				if got := m.Activity(repo.ID); got != registry.StageDocs {
					t.Errorf("activity = %q", got)
				}
				progress.Report(ctx, 2, 5)
				if _, has := m.Progress(repo.ID); has != report {
					t.Errorf("progress present = %t, want %t", has, report)
				}
				return manifest, wantErr
			})}
			got, err := m.runDocsStage(context.Background(), repo, d, dir, nil, docsStageOptions{force: !report, reportProgress: report})
			if got != manifest || !errors.Is(err, wantErr) {
				t.Fatalf("result = %v, %v", got, err)
			}
			if m.Activity(repo.ID) != "" {
				t.Fatal("stage leaked activity")
			}
			if _, has := m.Progress(repo.ID); has {
				t.Fatal("stage leaked progress")
			}
			stored, err := reg.Get(context.Background(), repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			st := stored.Stages.Docs
			if st.Status != registry.StageError || st.Error != wantErr.Error() || st.Commit != repo.LastCommit || st.FinishedAt.IsZero() {
				t.Fatalf("failed stage not persisted: %+v", st)
			}
		})
	}
}

func TestSharedStagesRejectDisabledCapabilities(t *testing.T) {
	for _, name := range []string{registry.StageCodeIndex, registry.StageDocs, registry.StageDocsIndex} {
		t.Run(name, func(t *testing.T) {
			m, _ := newStageTestManager(t)
			m.activity = map[string]map[string]struct{}{}
			repo := &registry.Repo{ID: "owner/repo"}
			d := &docsBundle{}
			ctx := context.Background()
			var err error
			switch name {
			case registry.StageCodeIndex:
				err = m.runCodeIndexStage(ctx, repo, d, codeIndexOptions{})
			case registry.StageDocs:
				_, err = m.runDocsStage(ctx, repo, d, "", nil, docsStageOptions{})
			case registry.StageDocsIndex:
				err = m.runDocsIndexStage(ctx, repo, d, "", nil)
			}
			if err == nil || repo.Stages.Get(name).Status != registry.StageError {
				t.Fatalf("disabled stage result = %v, state = %+v", err, repo.Stages.Get(name))
			}
		})
	}
}
