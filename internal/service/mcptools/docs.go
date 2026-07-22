package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/settings"
)

// ---- docs + RAG tools -------------------------------------------------------

type searchDocsArgs struct {
	Question string `json:"question" jsonschema:"natural language question to find relevant documentation for"`
	Repo     string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to search; omit to search across all repos"`
	TopDocs  int    `json:"top_docs,omitempty" jsonschema:"number of whole documents to return (default 5)"`
}

type searchCodeArgs struct {
	Query   string `json:"query" jsonschema:"text, symbol, path, natural-language or code query"`
	Repo    string `json:"repo,omitempty" jsonschema:"repository id (owner/name) to search; omit to search across all repos"`
	Mode    string `json:"mode,omitempty" jsonschema:"search mode: 'normal' for bw full-text search (default) or 'semantic' for vector search"`
	Page    int    `json:"page,omitempty" jsonschema:"normal mode page number (default 1)"`
	PerPage int    `json:"per_page,omitempty" jsonschema:"normal mode results per page (default 20, max 100)"`
	TopK    int    `json:"top_k,omitempty" jsonschema:"semantic mode source snippets to return (default 10)"`
}

func (a searchCodeArgs) searchMode() (string, error) {
	if a.Mode == "" {
		return "normal", nil
	}
	if a.Mode != "normal" && a.Mode != "semantic" {
		return "", fmt.Errorf("mode must be normal or semantic")
	}

	return a.Mode, nil
}

type listDocsArgs struct {
	Repo string `json:"repo" jsonschema:"repository id (owner/name) whose generated docs to list"`
}

type getDocArgs struct {
	Repo string `json:"repo" jsonschema:"repository id (owner/name) that owns the document"`
	Path string `json:"path" jsonschema:"doc path relative to the repo's krabby-docs/ dir, as returned by list_docs/search_docs"`
}

// addDocTools registers the documentation + RAG tools. They surface even when
// the subsystem is disabled; calls then return a clear 'not enabled' error.
func addDocTools(server *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_docs",
		Description: "RAG search over generated markdown documentation. Embeds the question, finds the " +
			"most relevant documents via the vector index, and returns the WHOLE markdown file(s) " +
			"so an LLM can answer from full context. Omit repo to search all repos.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args searchDocsArgs) (*mcp.CallToolResult, any, error) {
		docs, err := mgr.SearchDocs(ctx, args.Repo, args.Question, args.TopDocs)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(docs), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_code",
		Description: "Search raw source code using normal bw full-text search (default) or semantic vector search. " +
			"Returns ranked snippets with repository, path, symbol and line location. Normal mode supports exact " +
			"totals and pagination. Use read_file for broader context. Omit repo to search all repos.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args searchCodeArgs) (*mcp.CallToolResult, any, error) {
		mode, err := args.searchMode()
		if err != nil {
			return nil, nil, err
		}

		if mode == "normal" {
			result, err := mgr.SearchCodeText(ctx, args.Repo, args.Query, args.Page, args.PerPage)
			if err != nil {
				return nil, nil, err
			}

			return jsonResult(result), nil, nil
		}

		snippets, err := mgr.SearchCode(ctx, args.Repo, args.Query, args.TopK)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(map[string]any{
			"results":  snippets,
			"total":    len(snippets),
			"page":     1,
			"per_page": len(snippets),
		}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_docs",
		Description: "List the generated markdown documentation files for a repository (title + path + source).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listDocsArgs) (*mcp.CallToolResult, any, error) {
		docs, err := mgr.ListDocs(ctx, args.Repo)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(docs), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_doc",
		Description: "Return one whole generated markdown document by its repo-relative doc path.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args getDocArgs) (*mcp.CallToolResult, any, error) {
		doc, err := mgr.GetDoc(ctx, args.Repo, args.Path)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(doc), nil, nil
	})

	addDocConfigTools(server, mgr)
}

// ---- docs/RAG runtime configuration tools -----------------------------------

// setDocsConfigArgs mirrors the mutable settings. All fields are optional;
// secret fields (the API keys) are write-only and, when empty, keep the existing
// stored value. Timeouts are Go duration strings (e.g. "60s").
type setDocsConfigArgs struct {
	DocsEnabled     bool     `json:"docs_enabled,omitempty" jsonschema:"generate markdown docs in the refresh pipeline"`
	DocsConcurrency int      `json:"docs_concurrency,omitempty" jsonschema:"parallel per-file LLM summary calls"`
	DocsInclude     []string `json:"docs_include,omitempty" jsonschema:"source globs to document (repo-relative)"`
	DocsExclude     []string `json:"docs_exclude,omitempty" jsonschema:"source globs to skip"`
	DocsPrompt      string   `json:"docs_prompt,omitempty" jsonschema:"system prompt for the final documentation synthesis (empty uses the built-in default)"`

	LLMBaseURL string `json:"llm_base_url,omitempty" jsonschema:"OpenAI-compatible chat base URL, e.g. https://api.openai.com/v1"`
	LLMAPIKey  string `json:"llm_api_key,omitempty" jsonschema:"chat API key (write-only; leave empty to keep existing)"`
	LLMModel   string `json:"llm_model,omitempty" jsonschema:"chat model name"`
	LLMTimeout string `json:"llm_timeout,omitempty" jsonschema:"chat request timeout as a Go duration, e.g. 60s"`

	EmbedBaseURL string `json:"embed_base_url,omitempty" jsonschema:"OpenAI-compatible embeddings base URL, e.g. http://localhost:11434/v1"`
	EmbedAPIKey  string `json:"embed_api_key,omitempty" jsonschema:"embeddings API key (write-only; leave empty to keep existing)"`
	EmbedModel   string `json:"embed_model,omitempty" jsonschema:"embedding model name"`
	EmbedDim     int    `json:"embed_dim,omitempty" jsonschema:"embedding dimension (0 = infer)"`
	EmbedBatch   int    `json:"embed_batch,omitempty" jsonschema:"inputs per embeddings request"`
	EmbedTimeout string `json:"embed_timeout,omitempty" jsonschema:"embeddings request timeout as a Go duration, e.g. 30s"`

	CodeEmbedBaseURL string `json:"code_embed_base_url,omitempty" jsonschema:"dedicated code embeddings base URL; blank uses the docs embedder"`
	CodeEmbedAPIKey  string `json:"code_embed_api_key,omitempty" jsonschema:"code embeddings API key (write-only; leave empty to keep existing)"`
	CodeEmbedModel   string `json:"code_embed_model,omitempty" jsonschema:"code embedding model, e.g. codestral-embed-2505"`
	CodeEmbedDim     int    `json:"code_embed_dim,omitempty" jsonschema:"code embedding dimension (0 = infer)"`
	CodeEmbedBatch   int    `json:"code_embed_batch,omitempty" jsonschema:"code inputs per embeddings request"`
	CodeEmbedTimeout string `json:"code_embed_timeout,omitempty" jsonschema:"code embeddings request timeout as a Go duration, e.g. 30s"`

	RAGEnabled      bool   `json:"rag_enabled,omitempty" jsonschema:"enable indexing + retrieval"`
	RAGChunkSize    int    `json:"rag_chunk_size,omitempty" jsonschema:"target chunk length in characters"`
	RAGChunkOverlap int    `json:"rag_chunk_overlap,omitempty" jsonschema:"character overlap between chunks"`
	RAGTopK         int    `json:"rag_top_k,omitempty" jsonschema:"chunk matches fetched before grouping"`
	RAGTopDocs      int    `json:"rag_top_docs,omitempty" jsonschema:"whole documents returned"`
	StoreKind       string `json:"store_kind,omitempty" jsonschema:"vector store backend: 'embedded' or 'qdrant'"`

	CodeRAGEnabled      bool     `json:"code_rag_enabled,omitempty" jsonschema:"enable source-code indexing and semantic search"`
	CodeRAGChunkSize    int      `json:"code_rag_chunk_size,omitempty" jsonschema:"target code chunk length in characters"`
	CodeRAGChunkOverlap int      `json:"code_rag_chunk_overlap,omitempty" jsonschema:"character overlap for fallback code chunks"`
	CodeRAGTopK         int      `json:"code_rag_top_k,omitempty" jsonschema:"source snippets returned by default"`
	CodeRAGInclude      []string `json:"code_rag_include,omitempty" jsonschema:"source globs to index (empty uses built-in source extensions)"`
	CodeRAGExclude      []string `json:"code_rag_exclude,omitempty" jsonschema:"source globs to skip"`

	QdrantURL        string `json:"qdrant_url,omitempty" jsonschema:"Qdrant base URL"`
	QdrantAPIKey     string `json:"qdrant_api_key,omitempty" jsonschema:"Qdrant API key (write-only; leave empty to keep existing)"`
	QdrantCollection string `json:"qdrant_collection,omitempty" jsonschema:"Qdrant collection name"`
}

// merge overlays only JSON properties actually sent by the MCP client. The
// typed args alone cannot distinguish omitted fields from explicit zero values.
func (a setDocsConfigArgs) merge(base settings.Settings, raw json.RawMessage) (settings.Settings, error) {
	patch, err := a.patch(raw)
	if err != nil {
		return settings.Settings{}, err
	}

	return patch.Apply(base), nil
}

func (a setDocsConfigArgs) patch(raw json.RawMessage) (settings.Patch, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return settings.Patch{}, fmt.Errorf("decode settings fields; %w", err)
	}

	// settings.Patch handles presence for all fields except duration strings,
	// which MCP exposes in human-readable Go syntax (e.g. "30s").
	durationPresent := map[string]bool{}
	for _, key := range []string{"llm_timeout", "embed_timeout", "code_embed_timeout"} {
		_, durationPresent[key] = fields[key]
		delete(fields, key)
	}

	b, err := json.Marshal(fields)
	if err != nil {
		return settings.Patch{}, err
	}

	var patch settings.Patch
	if err := json.Unmarshal(b, &patch); err != nil {
		return settings.Patch{}, fmt.Errorf("decode settings patch; %w", err)
	}

	for key, value := range map[string]struct {
		raw string
		set func(time.Duration)
	}{
		"llm_timeout":        {a.LLMTimeout, func(d time.Duration) { patch.LLMTimeout = &d }},
		"embed_timeout":      {a.EmbedTimeout, func(d time.Duration) { patch.EmbedTimeout = &d }},
		"code_embed_timeout": {a.CodeEmbedTimeout, func(d time.Duration) { patch.CodeEmbedTimeout = &d }},
	} {
		if !durationPresent[key] {
			continue
		}

		d, err := time.ParseDuration(value.raw)
		if err != nil {
			return settings.Patch{}, fmt.Errorf("invalid %s: %w", key, err)
		}
		value.set(d)
	}

	return patch, nil
}

func settingsForArgs(ctx context.Context, mgr *manager.Manager, req *mcp.CallToolRequest, args setDocsConfigArgs) (settings.Settings, error) {
	current, err := mgr.GetDocsConfig(ctx)
	if err != nil {
		return settings.Settings{}, err
	}

	return args.merge(current.Settings, req.Params.Arguments)
}

func addDocConfigTools(server *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_docs_config",
		Description: "Return the current docs/RAG configuration (LLM, embedder, vector store, chunking). " +
			"Secrets are never returned; only *_key_set booleans indicate whether each API key is set.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		cfg, err := mgr.GetDocsConfig(ctx)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(cfg), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "set_docs_config",
		Description: "Update the docs/RAG configuration and rebuild the clients live (no restart). " +
			"API key fields are write-only: leave them empty to keep the existing secret. " +
			"Returns the redacted resulting config; a rebuild error is reported while the previous " +
			"working configuration stays active.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args setDocsConfigArgs) (*mcp.CallToolResult, any, error) {
		patch, err := args.patch(req.Params.Arguments)
		if err != nil {
			return nil, nil, err
		}

		cfg, err := mgr.PatchDocsConfig(ctx, patch)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(cfg), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "test_llm",
		Description: "Test the chat LLM connection and credentials without saving. Uses any provided " +
			"fields; blank api key falls back to the stored secret. Returns ok/latency/error.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args setDocsConfigArgs) (*mcp.CallToolResult, any, error) {
		merged, err := settingsForArgs(ctx, mgr, req, args)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(mgr.TestLLM(ctx, merged)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "test_embedder",
		Description: "Test the embeddings connection and credentials without saving. Uses any provided " +
			"fields; blank api key falls back to the stored secret. Returns ok/dim/latency/error.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args setDocsConfigArgs) (*mcp.CallToolResult, any, error) {
		merged, err := settingsForArgs(ctx, mgr, req, args)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(mgr.TestEmbedder(ctx, merged)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "test_code_embedder",
		Description: "Test the dedicated code embeddings connection without saving. A blank code " +
			"base URL uses the docs embedder. Returns ok/dim/latency/error.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args setDocsConfigArgs) (*mcp.CallToolResult, any, error) {
		merged, err := settingsForArgs(ctx, mgr, req, args)
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(mgr.TestCodeEmbedder(ctx, merged)), nil, nil
	})
}
