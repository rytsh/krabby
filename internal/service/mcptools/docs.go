package mcptools

import (
	"context"
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
	DocsConcurrency int      `json:"docs_concurrency,omitempty" jsonschema:"parallel per-file LLM doc calls"`
	DocsInclude     []string `json:"docs_include,omitempty" jsonschema:"source globs to document (repo-relative)"`
	DocsExclude     []string `json:"docs_exclude,omitempty" jsonschema:"source globs to skip"`
	DocsPrompt      string   `json:"docs_prompt,omitempty" jsonschema:"system prompt for per-file doc generation (empty uses the built-in default)"`

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

	RAGEnabled      bool   `json:"rag_enabled,omitempty" jsonschema:"enable indexing + retrieval"`
	RAGChunkSize    int    `json:"rag_chunk_size,omitempty" jsonschema:"target chunk length in characters"`
	RAGChunkOverlap int    `json:"rag_chunk_overlap,omitempty" jsonschema:"character overlap between chunks"`
	RAGTopK         int    `json:"rag_top_k,omitempty" jsonschema:"chunk matches fetched before grouping"`
	RAGTopDocs      int    `json:"rag_top_docs,omitempty" jsonschema:"whole documents returned"`
	StoreKind       string `json:"store_kind,omitempty" jsonschema:"vector store backend: 'embedded' or 'qdrant'"`

	QdrantURL        string `json:"qdrant_url,omitempty" jsonschema:"Qdrant base URL"`
	QdrantAPIKey     string `json:"qdrant_api_key,omitempty" jsonschema:"Qdrant API key (write-only; leave empty to keep existing)"`
	QdrantCollection string `json:"qdrant_collection,omitempty" jsonschema:"Qdrant collection name"`
}

func (a setDocsConfigArgs) toSettings() settings.Settings {
	parseDur := func(s string) time.Duration {
		d, _ := time.ParseDuration(s)

		return d
	}

	return settings.Settings{
		DocsEnabled:     a.DocsEnabled,
		DocsConcurrency: a.DocsConcurrency,
		DocsInclude:     a.DocsInclude,
		DocsExclude:     a.DocsExclude,
		DocsPrompt:      a.DocsPrompt,

		LLMBaseURL: a.LLMBaseURL,
		LLMAPIKey:  a.LLMAPIKey,
		LLMModel:   a.LLMModel,
		LLMTimeout: parseDur(a.LLMTimeout),

		EmbedBaseURL: a.EmbedBaseURL,
		EmbedAPIKey:  a.EmbedAPIKey,
		EmbedModel:   a.EmbedModel,
		EmbedDim:     a.EmbedDim,
		EmbedBatch:   a.EmbedBatch,
		EmbedTimeout: parseDur(a.EmbedTimeout),

		RAGEnabled:      a.RAGEnabled,
		RAGChunkSize:    a.RAGChunkSize,
		RAGChunkOverlap: a.RAGChunkOverlap,
		RAGTopK:         a.RAGTopK,
		RAGTopDocs:      a.RAGTopDocs,
		StoreKind:       a.StoreKind,

		QdrantURL:        a.QdrantURL,
		QdrantAPIKey:     a.QdrantAPIKey,
		QdrantCollection: a.QdrantCollection,
	}
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args setDocsConfigArgs) (*mcp.CallToolResult, any, error) {
		cfg, err := mgr.SetDocsConfig(ctx, args.toSettings())
		if err != nil {
			return nil, nil, err
		}

		return jsonResult(cfg), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "test_llm",
		Description: "Test the chat LLM connection and credentials without saving. Uses any provided " +
			"fields; blank api key falls back to the stored secret. Returns ok/latency/error.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args setDocsConfigArgs) (*mcp.CallToolResult, any, error) {
		return jsonResult(mgr.TestLLM(ctx, args.toSettings())), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "test_embedder",
		Description: "Test the embeddings connection and credentials without saving. Uses any provided " +
			"fields; blank api key falls back to the stored secret. Returns ok/dim/latency/error.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args setDocsConfigArgs) (*mcp.CallToolResult, any, error) {
		return jsonResult(mgr.TestEmbedder(ctx, args.toSettings())), nil, nil
	})
}
