package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rytsh/krabby/internal/service/bigpicture"
)

type pictureReadService interface {
	ListBigPictures(context.Context, bigpicture.ListOptions) (bigpicture.Page, error)
	BigPicture(context.Context, string) (*bigpicture.Picture, error)
	BigPictureSnapshot(context.Context, string, string) (*bigpicture.Snapshot, error)
	ReadBigPictureDocument(context.Context, string, string, string, int64, int) (*bigpicture.DocumentRead, error)
}

type pictureAdminService interface {
	TriggerBigPictureGeneration(context.Context, string) error
	SaveBigPicture(context.Context, string, bigpicture.Config) (*bigpicture.Picture, error)
	DeleteBigPicture(context.Context, string, uint64) error
	PublishBigPicture(context.Context, string, bigpicture.Publication) (*bigpicture.Snapshot, error)
}

type listPicturesArgs struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"default when omitted; '*' lists every namespace. Grouping only, not an access-control boundary"`
	Query     string `json:"query,omitempty" jsonschema:"filter workspace names, titles and descriptions"`
	Page      int    `json:"page,omitempty"`
	PerPage   int    `json:"per_page,omitempty" jsonschema:"default 20, max 100"`
}

type getPictureArgs struct {
	Name     string `json:"name" jsonschema:"exact big picture name from list_big_pictures"`
	Revision string `json:"revision,omitempty" jsonschema:"pin a publication ID; omitted selects current"`
	Path     string `json:"path,omitempty" jsonschema:"omit for workspace and document manifest; set a manifest path to read that document"`
	Offset   int64  `json:"offset,omitempty" jsonschema:"byte offset for document continuation"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"default 32768, max 131072"`
}

type pictureInspect struct {
	Workspace *bigpicture.Picture      `json:"workspace,omitempty"`
	Snapshot  *bigpicture.Snapshot     `json:"snapshot,omitempty"`
	Document  *bigpicture.DocumentRead `json:"document,omitempty"`
	Note      string                   `json:"note"`
}

func addPictureTools(server *mcp.Server, service pictureReadService) {
	addTool(server, &mcp.Tool{Name: "list_big_pictures", Description: "List architecture workspaces by namespace. This searches metadata, not document content.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, args listPicturesArgs) (*mcp.CallToolResult, bigpicture.Page, error) {
			page, err := service.ListBigPictures(ctx, bigpicture.ListOptions{Namespace: args.Namespace, Query: args.Query, Page: args.Page, PerPage: args.PerPage})
			return nil, page, err
		})
	addTool(server, &mcp.Tool{Name: "get_big_picture", Description: "Inspect a workspace and its document manifest, or read one published document. Pin the returned revision on continuation reads. Publications and citations are publisher-supplied, not verified live state.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, args getPictureArgs) (*mcp.CallToolResult, pictureInspect, error) {
			out := pictureInspect{Note: "Publisher-supplied architecture snapshot; verify claims against cited sources. Krabby has not independently validated citations."}
			if args.Path != "" {
				doc, err := service.ReadBigPictureDocument(ctx, args.Name, args.Revision, args.Path, args.Offset, args.MaxBytes)
				out.Document = doc
				return nil, out, err
			}
			p, err := service.BigPicture(ctx, args.Name)
			if err != nil {
				return nil, out, err
			}
			out.Workspace = p
			if p.CurrentRevision != "" || args.Revision != "" {
				out.Snapshot, err = service.BigPictureSnapshot(ctx, args.Name, args.Revision)
			} else {
				out.Note = "No documents published yet. Use generate_big_picture on the admin MCP for bounded research."
			}
			return nil, out, err
		})
}

type savePictureArgs struct {
	ExistingName string `json:"existing_name,omitempty" jsonschema:"omit to create; set to update that immutable workspace name"`
	Config       string `json:"config" jsonschema:"JSON object string: name,title,namespace,description,prompt,sources:[{kind:repo|web|api|mcp,ref}],expected_version,schedule:[cron specs]. Schedule authorizes recurring source disclosure to model/traces; [] disables, omitted keeps the saved schedule. Update requires current version"`
}
type publishPictureArgs struct {
	Name        string `json:"name"`
	Publication string `json:"publication" jsonschema:"JSON object string: expected_version, expected_revision (empty initially), producer, overview (document path), documents:[{path,title,markdown,evidence:[{source:{kind,ref},locator,revision}]}]. Replaces the whole document tree; max 64 documents, 256 KiB each, 4 MiB total"`
}
type deletePictureArgs struct {
	Name            string `json:"name"`
	ExpectedVersion uint64 `json:"expected_version" jsonschema:"current workspace version from get_big_picture"`
}

func parsePictureJSON(text string, maxBytes int, dst any) error {
	if len(text) > maxBytes {
		return fmt.Errorf("%w: JSON payload exceeds size limit", bigpicture.ErrInvalid)
	}
	// Do not return decoder errors: document text and prompts can be sensitive.
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return bigpicture.ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return bigpicture.ErrInvalid
	}
	return nil
}

func addPictureAdminTools(server *mcp.Server, service pictureAdminService) {
	addTool(server, &mcp.Tool{Name: "generate_big_picture", Description: "Queue bounded architecture research using repo files, cached Source/API docs and granted exact MCP resource URIs; no external tools/templates. Uses the documentation chat model; failure preserves the publication. Monitor queue_status for bigpicture:<name>."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args struct {
			Name string `json:"name"`
		}) (*mcp.CallToolResult, any, error) {
			err := service.TriggerBigPictureGeneration(ctx, args.Name)
			return nil, map[string]string{"status": "queued", "scope": "bigpicture:" + args.Name}, err
		})
	addTool(server, &mcp.Tool{Name: "save_big_picture", Description: "Create or replace architecture workspace settings and explicit source selections. Does not generate documents or access external MCPs."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args savePictureArgs) (*mcp.CallToolResult, *bigpicture.Picture, error) {
			var cfg bigpicture.Config
			if err := parsePictureJSON(args.Config, 96<<10, &cfg); err != nil {
				return nil, nil, err
			}
			p, err := service.SaveBigPicture(ctx, args.ExistingName, cfg)
			return nil, p, err
		})
	addTool(server, &mcp.Tool{Name: "publish_big_picture", Description: "Publish a complete Markdown document tree supplied by the caller, preserving prior revisions. Requires current config version and publication ID; failed validation leaves existing documents unchanged. No model call or source verification is performed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args publishPictureArgs) (*mcp.CallToolResult, *bigpicture.Snapshot, error) {
			var pub bigpicture.Publication
			if err := parsePictureJSON(args.Publication, 8<<20, &pub); err != nil {
				return nil, nil, err
			}
			snapshot, err := service.PublishBigPicture(ctx, args.Name, pub)
			return nil, snapshot, err
		})
	destructive := true
	addTool(server, &mcp.Tool{Name: "delete_big_picture", Description: "Delete an architecture workspace and its publications. Does not delete source repositories or connections.", Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive}},
		func(ctx context.Context, _ *mcp.CallToolRequest, args deletePictureArgs) (*mcp.CallToolResult, any, error) {
			if args.ExpectedVersion == 0 {
				return nil, nil, bigpicture.ErrConflict
			}
			err := service.DeleteBigPicture(ctx, args.Name, args.ExpectedVersion)
			return nil, map[string]bool{"ok": err == nil}, err
		})
}
