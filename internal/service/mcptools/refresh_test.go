package mcptools

import (
	"testing"

	"github.com/rytsh/krabby/internal/service/registry"
)

func TestRefreshRepoArgsValidateStages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stages  []string
		wantErr bool
	}{
		{name: "empty is full pipeline", stages: nil},
		{name: "single valid", stages: []string{registry.StageDocsIndex}},
		{
			name: "all valid",
			stages: []string{
				registry.StageGraph,
				registry.StageDocs,
				registry.StageDocsIndex,
				registry.StageCodeIndex,
			},
		},
		{name: "unknown stage", stages: []string{"docs_indx"}, wantErr: true},
		{name: "mixed valid and invalid", stages: []string{registry.StageDocs, "bogus"}, wantErr: true},
		{name: "sync is not a generate stage", stages: []string{"sync"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := refreshRepoArgs{Repo: "owner/repo", Stages: tt.stages}.validateStages()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateStages(%v) error = %v, wantErr %v", tt.stages, err, tt.wantErr)
			}
		})
	}
}
