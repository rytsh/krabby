package mcptools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/registry"
)

func TestRepoOverrideArgsValidateSkipStages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
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
		// registry.Overrides.Normalize drops these, which would make the call
		// succeed while the stage keeps running and keeps spending LLM budget.
		{name: "unknown stage", skip: []string{"docs_gen"}, wantErr: true},
		{name: "mixed valid and invalid", skip: []string{registry.StageDocs, "bogus"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := repoOverrideArgs{SkipStages: tt.skip}.validateSkipStages()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSkipStages(%v) error = %v, wantErr %v", tt.skip, err, tt.wantErr)
			}
		})
	}
}

// errStubReached is returned by every stub mutation, so a tool call that got
// past argument validation is distinguishable from one rejected at the surface.
var errStubReached = errors.New("stub service reached")

// stubRepoService satisfies the read, admin and queue boundaries of
// addManagementTools without a registry or a queue behind it. The real Manager
// cannot be used here: manager.New with a nil registry panics inside AddRepo
// before any argument is looked at.
type stubRepoService struct {
	overrides []registry.Overrides
}

func (s *stubRepoService) ListRepos(context.Context, registry.ListOptions) ([]*registry.Repo, int, error) {
	return nil, 0, nil
}

func (s *stubRepoService) RepoNamespaces(context.Context) ([]registry.NamespaceGroup, error) {
	return nil, nil
}

func (s *stubRepoService) Repo(context.Context, string) (*registry.Repo, error) { return nil, nil }
func (s *stubRepoService) Activity(string) string                               { return "" }

func (s *stubRepoService) AddRepo(_ context.Context, spec manager.RepoSpec, _ ...string) (*registry.Repo, error) {
	s.overrides = append(s.overrides, spec.Overrides)

	return nil, errStubReached
}

func (s *stubRepoService) AddRepoWait(_ context.Context, spec manager.RepoSpec, _ ...string) (*registry.Repo, bool, error) {
	s.overrides = append(s.overrides, spec.Overrides)

	return nil, false, errStubReached
}

func (s *stubRepoService) SetRepoNamespace(context.Context, string, string) (*registry.Repo, error) {
	return nil, errStubReached
}

func (s *stubRepoService) SetRepoOverrides(_ context.Context, _ string, over registry.Overrides) (*registry.Repo, error) {
	s.overrides = append(s.overrides, over)

	return nil, errStubReached
}

func (s *stubRepoService) UpsertNamespace(context.Context, string, types.Null[string]) (*registry.NamespaceRecord, error) {
	return nil, errStubReached
}

func (s *stubRepoService) DeleteNamespace(context.Context, string) error { return errStubReached }
func (s *stubRepoService) RemoveRepo(context.Context, string) error      { return errStubReached }
func (s *stubRepoService) TriggerGenerate(string, []string, bool) error  { return errStubReached }

func (s *stubRepoService) GenerateWait(context.Context, string, []string, bool) (*registry.Repo, bool, error) {
	return nil, false, errStubReached
}

func (s *stubRepoService) TriggerRefresh(string, ...string) error { return errStubReached }

func (s *stubRepoService) RefreshWait(context.Context, string, ...string) (*registry.Repo, bool, error) {
	return nil, false, errStubReached
}

func (s *stubRepoService) CancelJob(string) bool        { return false }
func (s *stubRepoService) TaskSnapshot() queue.Snapshot { return queue.Snapshot{} }
func (s *stubRepoService) BumpTask(uint64) bool         { return false }
func (s *stubRepoService) CancelTask(uint64) bool       { return false }
func (s *stubRepoService) CancelTasks(string) int       { return 0 }
func (s *stubRepoService) SetTaskConcurrency(int)       {}

// adminRepoSession registers the admin management tools against a stub service
// and connects an in-memory MCP client to them.
func adminRepoSession(t *testing.T) (*mcp.ClientSession, *stubRepoService) {
	t.Helper()

	stub := &stubRepoService{}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	addManagementTools(server, stub, stub, stub, 0, true)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session, stub
}

// callToolText returns a tool call's concatenated text content and whether the
// call was reported as an error.
func callToolText(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) transport error = %v", name, err)
	}

	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}

	return sb.String(), result.IsError
}

// A skip_stages typo must be refused by the tool that accepted it. Both tools
// store the list through registry.Overrides, whose Normalize drops unknown
// names, so without validation at the surface the caller is told the stage was
// skipped while it keeps running.
func TestRepoOverrideToolsRejectUnknownSkipStage(t *testing.T) {
	for _, tt := range []struct {
		tool string
		args map[string]any
	}{
		{
			tool: "add_repo",
			args: map[string]any{"url": "https://example.com/team/repo.git", "skip_stages": []string{"docs_gen"}},
		},
		{
			// wait=true takes the other AddRepo path, which must reject too.
			tool: "add_repo",
			args: map[string]any{
				"url": "https://example.com/team/repo.git", "wait": true,
				"skip_stages": []string{"docs_gen"},
			},
		},
		{
			tool: "set_repo_overrides",
			args: map[string]any{"repo": "example.com/team/repo", "skip_stages": []string{"docs_gen"}},
		},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			session, stub := adminRepoSession(t)

			text, isErr := callToolText(t, session, tt.tool, tt.args)
			if !isErr {
				t.Fatalf("%s accepted skip_stages=['docs_gen']: %s", tt.tool, text)
			}
			if len(stub.overrides) != 0 {
				t.Fatalf("%s reached the service with %+v", tt.tool, stub.overrides)
			}
			if !strings.Contains(text, `unknown stage "docs_gen"`) {
				t.Fatalf("%s error = %q, want it to name the unknown stage", tt.tool, text)
			}
			for _, stage := range []string{
				registry.StageGraph, registry.StageDocs, registry.StageDocsIndex, registry.StageCodeIndex,
			} {
				if !strings.Contains(text, stage) {
					t.Errorf("%s error = %q, want it to list valid stage %q", tt.tool, text, stage)
				}
			}
		})
	}
}

// Every name in registry.ValidStage's vocabulary must still reach the service:
// the check rejects typos, not the feature.
func TestRepoOverrideToolsAcceptEveryValidSkipStage(t *testing.T) {
	stages := []string{
		registry.StageGraph, registry.StageDocs, registry.StageDocsIndex, registry.StageCodeIndex,
	}

	// Each stage on its own, then the whole vocabulary at once - which is how a
	// caller tracks a repo it only wants cloned.
	cases := make([][]string, 0, len(stages)+1)
	for _, stage := range stages {
		cases = append(cases, []string{stage})
	}
	cases = append(cases, stages)

	for _, skip := range cases {
		for _, tt := range []struct {
			tool string
			args map[string]any
		}{
			{tool: "add_repo", args: map[string]any{"url": "https://example.com/team/repo.git"}},
			{tool: "set_repo_overrides", args: map[string]any{"repo": "example.com/team/repo"}},
		} {
			t.Run(tt.tool+"/"+strings.Join(skip, ","), func(t *testing.T) {
				session, stub := adminRepoSession(t)

				args := map[string]any{"skip_stages": skip}
				for k, v := range tt.args {
					args[k] = v
				}

				text, _ := callToolText(t, session, tt.tool, args)
				if len(stub.overrides) != 1 {
					t.Fatalf("%s rejected skip_stages=%v: %s", tt.tool, skip, text)
				}
				if got := stub.overrides[0].SkipStages; strings.Join(got, ",") != strings.Join(skip, ",") {
					t.Fatalf("%s forwarded SkipStages = %v, want %v", tt.tool, got, skip)
				}
			})
		}
	}
}
