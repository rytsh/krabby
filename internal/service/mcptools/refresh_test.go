package mcptools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/manager"
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

func TestRefreshRepoArgsValidateSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stages  []string
		skip    []string
		wantErr bool
	}{
		{name: "empty skips nothing"},
		{name: "single valid", skip: []string{registry.StageDocs}},
		{
			name: "all valid",
			skip: []string{
				registry.StageGraph,
				registry.StageDocs,
				registry.StageDocsIndex,
				registry.StageCodeIndex,
			},
		},
		{name: "unknown stage", skip: []string{"documentation"}, wantErr: true},
		{name: "mixed valid and invalid", skip: []string{registry.StageDocs, "bogus"}, wantErr: true},
		{
			// stages is an allow-list against the existing clone, skip a
			// deny-list on the full pull+rebuild: both together has no meaning.
			name:    "skip cannot be combined with stages",
			stages:  []string{registry.StageDocs},
			skip:    []string{registry.StageGraph},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := refreshRepoArgs{Repo: "owner/repo", Stages: tt.stages, Skip: tt.skip}.validateSkip()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSkip(stages=%v, skip=%v) error = %v, wantErr %v",
					tt.stages, tt.skip, err, tt.wantErr)
			}
		})
	}
}

func TestRefreshRepoToolReportsEnqueueRejection(t *testing.T) {
	mgr := manager.New(context.Background(), nil, nil, nil, nil, nil, nil, "", "", false, manager.DocsDeps{})
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}

	server := NewAdmin(mgr, "test", 0)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	result, callErr := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "refresh_repo",
		Arguments: map[string]any{"repo": "owner/repo"},
	})
	if callErr != nil {
		if !strings.Contains(callErr.Error(), "queue is closed") {
			t.Fatalf("CallTool error = %v, want queue rejection", callErr)
		}

		return
	}
	if result == nil || !result.IsError {
		t.Fatalf("CallTool result = %+v, want tool error", result)
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "queue is closed") {
		t.Fatalf("CallTool content = %+v, want queue rejection", result.Content)
	}
}
