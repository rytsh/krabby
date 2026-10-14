package mcptools

import (
	"context"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/memory"
)

const memoryInstructions = `This server is Krabby's shared, durable memory for agents and people. Memories are Markdown notes grouped in spaces; every client can read every space, so never store secrets or personal data.

- Before non-trivial work in a repository, call recall with the task and repo to reuse decisions, conventions and gotchas.
- Call remember only for durable, non-obvious knowledge future sessions need: why a decision was made, a convention, a pitfall. Do not store task progress, code that is already in the repo, or anything a search would find.
- Pin evidence (repo + path, optionally lines or a symbol) when a memory is about code. Krabby re-checks pins after every repository refresh and marks memories stale or orphaned when the code changes or disappears. Verify stale memories with read_file before trusting them, then update_memory to confirm or correct them.
- Spaces: 'default' is shared team knowledge. When the user names a section ("add this to development notes"), use that name as the space, lowercase ('development'); put sub-topics in tags ('migration') or a sub-space ('development/migration'). Reuse existing space names: check list_memories when unsure, and if remember reports new_space for a space you did not intend, move the memory with update_memory. recall searches all spaces unless you pass spaces.
- author is optional free text; set it only to a name the user gave you (a person, team or agent name). When omitted, the HTTP X-User header is used if present.
- remember returns an existing memory instead of a duplicate; refine it with update_memory.`

type memoryService interface {
	Remember(context.Context, memory.Input) (manager.RememberResult, error)
	RecallMemories(context.Context, string, string, memory.ListOptions, int) (manager.MemorySearch, error)
	Memory(context.Context, string) (*memory.Memory, error)
	UpdateMemory(context.Context, string, memory.Patch) (*memory.Memory, error)
	ForgetMemory(context.Context, string) error
	ListMemories(context.Context, memory.ListOptions) (memory.Page, error)
	MemorySpaces(context.Context) ([]memory.SpaceCount, error)
}

type memoryEvidenceArg struct {
	Repo      string `json:"repo" jsonschema:"tracked repository id"`
	Path      string `json:"path,omitempty" jsonschema:"repo-relative file; optional when symbol is given"`
	Symbol    string `json:"symbol,omitempty" jsonschema:"symbol name; without lines it resolves to its definition"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"1-based first line; omit to pin only the file's existence"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"inclusive last line"`
}

func evidenceInput(args []memoryEvidenceArg) []memory.Evidence {
	out := make([]memory.Evidence, 0, len(args))
	for _, a := range args {
		out = append(out, memory.Evidence{Repo: a.Repo, Path: a.Path, Symbol: a.Symbol, StartLine: a.StartLine, EndLine: a.EndLine})
	}
	return out
}

type rememberArgs struct {
	Title     string              `json:"title" jsonschema:"one-line summary, max 200 bytes"`
	Content   string              `json:"content" jsonschema:"Markdown body: the fact and why it matters, max 16 KiB"`
	Kind      string              `json:"kind,omitempty" jsonschema:"decision, convention, gotcha, fact, preference, todo or note (default)"`
	Space     string              `json:"space,omitempty" jsonschema:"section the user named, lowercase (e.g. 'development'); default 'default'. Reuse existing names"`
	Tags      []string            `json:"tags,omitempty"`
	Evidence  []memoryEvidenceArg `json:"evidence,omitempty" jsonschema:"code locations this memory is about; re-checked after each refresh"`
	ExpiresAt string              `json:"expires_at,omitempty" jsonschema:"RFC 3339 time after which the memory is hidden"`
	Author    string              `json:"author,omitempty" jsonschema:"optional name of who wrote it: a person, team or agent; defaults to HTTP X-User"`
}

type recallArgs struct {
	Query  string   `json:"query" jsonschema:"what you are about to work on or want to know"`
	Spaces []string `json:"spaces,omitempty" jsonschema:"limit to these spaces; omitted searches all"`
	Repo   string   `json:"repo,omitempty" jsonschema:"only memories with evidence in this repository"`
	Kind   string   `json:"kind,omitempty"`
	Tag    string   `json:"tag,omitempty"`
	Status string   `json:"status,omitempty" jsonschema:"active, stale, orphaned, expired or *; omitted hides expired"`
	Mode   string   `json:"mode,omitempty" jsonschema:"hybrid (default with embedder), semantic or lexical"`
	Limit  int      `json:"limit,omitempty" jsonschema:"default 10, max 50"`
}

type memoryIDArgs struct {
	ID string `json:"id" jsonschema:"memory id"`
}

type getMemoryArgs struct {
	ID     string `json:"id" jsonschema:"memory id"`
	Format string `json:"format,omitempty" jsonschema:"markdown (default) or json"`
}

type updateMemoryArgs struct {
	ID              string               `json:"id"`
	ExpectedVersion uint64               `json:"expected_version,omitempty" jsonschema:"version you read; rejects the update if someone changed it since"`
	Title           *string              `json:"title,omitempty"`
	Content         *string              `json:"content,omitempty" jsonschema:"replaces the whole Markdown body"`
	Kind            *string              `json:"kind,omitempty"`
	Space           *string              `json:"space,omitempty"`
	Tags            *[]string            `json:"tags,omitempty" jsonschema:"replaces all tags"`
	Evidence        *[]memoryEvidenceArg `json:"evidence,omitempty" jsonschema:"replaces all evidence; [] removes pins"`
	ExpiresAt       *string              `json:"expires_at,omitempty" jsonschema:"RFC 3339; empty string clears"`
	Note            string               `json:"note,omitempty" jsonschema:"why it changed; recorded in history"`
	Author          string               `json:"author,omitempty" jsonschema:"optional name of who made this change; defaults to HTTP X-User"`
}

type listMemoriesArgs struct {
	Spaces  []string `json:"spaces,omitempty" jsonschema:"omitted lists all spaces"`
	Repo    string   `json:"repo,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Tag     string   `json:"tag,omitempty"`
	Status  string   `json:"status,omitempty" jsonschema:"active, stale, orphaned, expired or *; omitted hides expired"`
	Page    int      `json:"page,omitempty"`
	PerPage int      `json:"per_page,omitempty" jsonschema:"default 20, max 100"`
}

// memoryBrief is the compact listing shape: history and full content stay out
// of search results so a recall does not flood the context.
type memoryBrief struct {
	ID        string            `json:"id"`
	Space     string            `json:"space"`
	Kind      string            `json:"kind"`
	Status    string            `json:"status"`
	Title     string            `json:"title"`
	Content   string            `json:"content"`
	Truncated bool              `json:"truncated,omitempty"`
	Tags      []string          `json:"tags,omitempty"`
	Evidence  []memory.Evidence `json:"evidence,omitempty"`
	Version   uint64            `json:"version"`
	UpdatedAt time.Time         `json:"updated_at"`
	Score     float64           `json:"score,omitempty"`
}

const briefContentBytes = 1200

func brief(m *memory.Memory, score float64) memoryBrief {
	content, truncated := m.Content, false
	if len(content) > briefContentBytes {
		cut := briefContentBytes
		for cut > 0 && !isRuneStart(content[cut]) {
			cut--
		}
		content, truncated = content[:cut]+"…", true
	}
	return memoryBrief{ID: m.ID, Space: m.Space, Kind: m.Kind, Status: m.Status, Title: m.Title, Content: content, Truncated: truncated,
		Tags: m.Tags, Evidence: m.Evidence, Version: m.Version, UpdatedAt: m.UpdatedAt, Score: score}
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, memory.ErrInvalid
	}
	return t, nil
}

// memoryAuthor uses the current request header only when no author was supplied.
// The memory service validates both explicit and header-derived names.
func memoryAuthor(author string, req *mcp.CallToolRequest) string {
	if strings.TrimSpace(author) != "" || req == nil || req.Extra == nil {
		return author
	}
	return req.Extra.Header.Get("X-User")
}

// NewMemory builds the shared memory MCP catalog.
func NewMemory(mgr *manager.Manager, version string) *mcp.Server {
	server := newServer(mgr, "krabby-memory", "Krabby shared memory", version, memoryInstructions)
	addMemoryTools(server, mgr)

	return server
}

func addMemoryTools(server *mcp.Server, svc memoryService) {
	addTool(server, &mcp.Tool{
		Name:        "remember",
		Description: "Store a durable Markdown memory for future sessions. Returns the existing memory instead when the same space already holds the same or a near-identical one.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args rememberArgs) (*mcp.CallToolResult, any, error) {
		expires, err := parseTime(args.ExpiresAt)
		if err != nil {
			return nil, nil, err
		}
		res, err := svc.Remember(ctx, memory.Input{Space: args.Space, Kind: args.Kind, Title: args.Title, Content: args.Content, Tags: args.Tags,
			Evidence: evidenceInput(args.Evidence), ExpiresAt: expires, Author: memoryAuthor(args.Author, req)})
		if err != nil {
			return nil, nil, err
		}
		out := map[string]any{"created": res.Created, "duplicate": res.Duplicate, "similarity": res.Score, "note": res.Note, "memory": brief(res.Memory, 0)}
		if res.NewSpace {
			out["new_space"], out["existing_spaces"] = true, res.Spaces
		}
		return jsonResult(out), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "recall",
		Description: "Search memories by meaning and keywords. Check status: stale or orphaned memories describe code that has since changed; verify before relying on them.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args recallArgs) (*mcp.CallToolResult, any, error) {
		res, err := svc.RecallMemories(ctx, args.Query, args.Mode, memory.ListOptions{Spaces: args.Spaces, Repo: args.Repo, Kind: args.Kind, Tag: args.Tag, Status: args.Status}, args.Limit)
		if err != nil {
			return nil, nil, err
		}
		items := make([]memoryBrief, 0, len(res.Results))
		for _, hit := range res.Results {
			items = append(items, brief(hit.Memory, hit.Score))
		}
		return jsonResult(map[string]any{"mode": res.Mode, "note": res.Note, "results": items}), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "get_memory",
		Description: "Read one memory in full, including evidence status and its timestamped history. Markdown by default.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args getMemoryArgs) (*mcp.CallToolResult, any, error) {
		m, err := svc.Memory(ctx, args.ID)
		if err != nil {
			return nil, nil, err
		}
		if strings.EqualFold(args.Format, "json") {
			return jsonResult(m), nil, nil
		}
		return textResult(memory.Markdown(m)), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "update_memory",
		Description: "Change selected fields of a memory; omitted fields are kept. Updating a stale memory re-pins its evidence to the current code, so check the code first.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args updateMemoryArgs) (*mcp.CallToolResult, any, error) {
		patch := memory.Patch{ExpectedVersion: args.ExpectedVersion, Space: args.Space, Kind: args.Kind, Title: args.Title, Content: args.Content, Tags: args.Tags, Note: args.Note, Author: memoryAuthor(args.Author, req)}
		if args.Evidence != nil {
			ev := evidenceInput(*args.Evidence)
			patch.Evidence = &ev
		}
		if args.ExpiresAt != nil {
			t, err := parseTime(*args.ExpiresAt)
			if err != nil {
				return nil, nil, err
			}
			patch.ExpiresAt = &t
		}
		m, err := svc.UpdateMemory(ctx, args.ID, patch)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(brief(m, 0)), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "forget",
		Description: "Permanently delete a memory that is wrong or no longer useful. Prefer update_memory when it only needs correcting.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args memoryIDArgs) (*mcp.CallToolResult, any, error) {
		if err := svc.ForgetMemory(ctx, args.ID); err != nil {
			return nil, nil, err
		}
		return textResult("forgot memory " + args.ID), nil, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "list_memories",
		Description: "Browse memories newest first, or list spaces with counts when called with no filters. Use recall to search.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listMemoriesArgs) (*mcp.CallToolResult, any, error) {
		page, err := svc.ListMemories(ctx, memory.ListOptions{Spaces: args.Spaces, Repo: args.Repo, Kind: args.Kind, Tag: args.Tag, Status: args.Status, Page: args.Page, PerPage: args.PerPage})
		if err != nil {
			return nil, nil, err
		}
		items := make([]memoryBrief, 0, len(page.Items))
		for _, m := range page.Items {
			items = append(items, brief(m, 0))
		}
		out := map[string]any{"items": items, "total": page.Total, "page": page.Page, "per_page": page.PerPage}
		if len(args.Spaces) == 0 && args.Repo == "" && args.Kind == "" && args.Tag == "" && args.Status == "" && args.Page <= 1 {
			spaces, err := svc.MemorySpaces(ctx)
			if err != nil {
				return nil, nil, err
			}
			out["spaces"] = spaces
		}
		return jsonResult(out), nil, nil
	})
}

func ptr[T any](v T) *T { return &v }
