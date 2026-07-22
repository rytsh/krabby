# Design: Docs + RAG subsystem

Status: **Implemented** — indexing (chunk → embed → upsert), file-level
retrieval, and the embedded bw/Badger HNSW vector store are in place.

## Goal

Extend krabby beyond code knowledge graphs with a documentation + retrieval layer:

1. **Repo → Markdown docs.** For every tracked repo, generate human-readable
   markdown documentation (per file / per package + an overview), using the
   existing graphify graph + source files, enriched by an **LLM**.
2. **Render docs in the UI** (existing Svelte app under `_ui/` → `internal/server/dist`) with
   full-text **search**.
3. **RAG index.** Chunk + embed the generated markdown into a **vector store**;
   on a question, retrieve the most relevant docs.
4. **File-level retrieval.** RAG is used only to *find which markdown files* are
   relevant. The whole file(s) are then returned to the caller/LLM — not just the
   matching chunks. So we keep two things: the **markdown files** (source of
   truth, served whole) and the **vector index** (a routing layer over them).
5. **New MCP tools + REST endpoints** exposing doc search / retrieval to agents
   and the UI.

## Non-goals (this pass)

- No answer synthesis inside krabby by default (krabby returns docs; the calling
  LLM composes the answer). An optional `ask_docs` convenience tool may call the
  chat LLM, but retrieval is the core.
- No re-implementation of graphify. Docs are a consumer of the graph, not a
  replacement.

## Design decisions (confirmed)

| Area | Decision |
| --- | --- |
| Doc generation | **LLM-generated** per file/package (chat completion). Pluggable behind a `docgen.Generator` interface so a deterministic fallback can be added. |
| Embeddings | **Pluggable OpenAI-compatible** HTTP embedder (`/v1/embeddings`). Works with OpenAI, Ollama, LM Studio, TEI, vLLM. |
| Vector store | **Embedded `vectorstore.Store` implementation:** dedicated bw (BadgerDB) databases under `data_dir/docs-vectors` and `data_dir/code-vectors`, each with an HNSW cosine index. The embedding dimension is auto-detected on first insert and a model/dimension change wipes + rebuilds the derived index. |
| LLM chat | **OpenAI-compatible** `/v1/chat/completions` client (shared config style with embedder). |

Rationale: mirrors krabby's existing "plain files under `data_dir`, external
process/HTTP integrations, pluggable" philosophy while preserving the
zero-infrastructure promise.

## Architecture

```
                      refresh pipeline (manager.refresh)
 git pull ─► graphify update ─► merge-graphs
                    │
                    ├─► docgen.Generate(repo)                 [NEW]
                    │      graph.json + source ──LLM──► markdown files
                    │      └─ writes ~/.krabby/docs/<o>/<n>/**.md
                    │                                    + docs-index.json (manifest)
                    │
                    └─► rag.Index(repo)                        [NEW]
                           for each doc: chunk ─► embedder ─► vectorstore.Upsert
                                                       (payload: repo, doc path, title)

 query path (MCP tool / REST):
   question ─► embedder.Embed(question) ─► vectorstore.Search(topK)
            ─► collect distinct doc paths (ranked) ─► read WHOLE markdown files
            ─► return [{repo, path, title, score, content}]  (file-level)
```

### New packages (`internal/service/...`)

| Package | Responsibility |
| --- | --- |
| `llm` | OpenAI-compatible chat client (`Complete(ctx, messages) (string, error)`). Used by docgen and optional `ask_docs`. |
| `embedder` | OpenAI-compatible embeddings client (`Embed(ctx, []string) ([][]float32, error)`), exposes `Dim()`, `Model()`. |
| `vectorstore` | `Store` interface + embedded bw/Badger HNSW backend. |
| `docgen` | `Generator` interface + `llmgen` impl: reads graph + source, prompts the LLM per file/package, writes markdown + a `docs-index.json` manifest. |
| `rag` | Orchestrates chunk → embed → upsert (indexing) and embed-query → search → whole-file fetch (retrieval). |

### Data layout

```
~/.krabby/repos/<owner>/<name>/
└── graphify-out/                 # existing; clones contain no generated docs
~/.krabby/docs/<owner>/<name>/
├── docs-index.json               # manifest: files, titles, source hash, model, generated_at
├── documentation.md              # comprehensive repository documentation
├── .summaries/                   # incremental per-source summary cache
└── ...
~/.krabby/docs-vectors/           # embedded docs store backend
└── ...                           # bw (BadgerDB) database: chunk records + HNSW vector index
```

Docs are plain markdown so external tools (and the UI) can read them directly,
consistent with the graphify artifacts. The vector store is a *derived index*;
it can always be rebuilt from the markdown.

### Store interface (sketch)

```go
type Store interface {
    // Upsert replaces all vectors for a repo (idempotent per rebuild).
    Upsert(ctx context.Context, repo string, items []Item) error
    // Search returns the topK nearest chunks, optionally filtered to one repo
    // (repo == "" searches across all).
    Search(ctx context.Context, repo string, vec []float32, topK int) ([]Match, error)
    // DeleteRepo drops all vectors for a repo (on remove/refresh).
    DeleteRepo(ctx context.Context, repo string) error
}

type Item struct {
    ID      string            // stable: repo + docPath + chunkIdx
    Vector  []float32
    Payload Payload
}
type Payload struct {
    Repo    string
    DocPath string   // path relative to the repo's external docs directory
    Title   string
    Chunk   string   // the chunk text (for optional chunk-level display)
}
type Match struct {
    Payload Payload
    Score   float32
}
```

### File-level retrieval detail

`rag.Retrieve` returns **docs**, not chunks:

1. Embed the question.
2. `Search` for `topK` chunks (topK larger than the doc count you want, e.g. 20).
3. Group matches by `DocPath`, score each doc = max (or sum) of its chunk scores.
4. Take the top N docs; read each **full** markdown file from the repo's directory under `data_dir/docs/`.
5. Return `[]Doc{Repo, Path, Title, Score, Content}`.

This satisfies "hold markdown + RAG for searching which markdown files relate to
the question, then use that markdown file directly".

## Runtime settings

These fields originally lived in `config.Config`; they now live in the
persisted `settings.Settings` record and are managed through the UI, REST API
or MCP tools. Changing them rebuilds the affected clients without restarting
krabby. The shape below is conceptual rather than YAML file configuration.

```yaml
docs:
  enabled: true
  concurrency: 4          # parallel LLM doc calls
  include: ["**/*.go"]    # globs of source files to document
  exclude: ["**/*_test.go", "vendor/**"]

llm:                      # OpenAI-compatible chat (docgen, ask_docs)
  base_url: "https://api.openai.com/v1"
  api_key: ""             # log:"-"
  model: "gpt-4o-mini"
  timeout: 60s

embedder:                 # OpenAI-compatible embeddings
  base_url: "http://localhost:11434/v1"   # e.g. ollama
  api_key: ""             # log:"-"
  model: "nomic-embed-text"
  dim: 768                # optional; verified against first response
  batch: 64
  timeout: 30s

rag:
  enabled: true
  chunk_size: 1200        # chars
  chunk_overlap: 200
  top_k: 20               # chunk matches fetched
  top_docs: 5             # whole docs returned
```

System-level `Config` helpers still derive storage paths such as
`DocsRootDir()` and `DocsVectorsDir()` from `data_dir`.

## Manager integration

- `refresh()` gains two optional steps after `gfy.Update()` succeeds and after
  `rebuildMerged`: `docgen.Generate` then `rag.Index`. Both are guarded by
  `docs.enabled` / `rag.enabled` and run best-effort (log on failure, don't fail
  the graph build). A new repo status value `StatusIndexing` may be added, or we
  reuse `StatusBuilding` with a sub-phase.
- `RemoveRepo()` also calls `store.DeleteRepo` and safely removes the repo's
  external generated-docs directory.
- New Manager methods (thin, delegate to rag/docgen):
  - `SearchDocs(ctx, repo, question, topDocs) ([]rag.Doc, error)`
  - `GetDoc(ctx, repo, docPath) (content, error)`
  - `ListDocs(ctx, repo) ([]docgen.DocMeta, error)`
  - `AskDocs(ctx, repo, question) (answer, sources, error)` (optional; uses llm)

## MCP tools (new)

| Tool | Purpose |
| --- | --- |
| `list_docs` | List generated doc files for a repo (or all). |
| `get_doc` | Return one whole markdown doc by path. |
| `search_docs` | RAG: return the top whole markdown docs relevant to a question. |
| `ask_docs` | (optional) RAG + chat LLM: answer a question, cite doc sources. |

All take optional `repo` (owner/name); omit = across all repos, mirroring the
graph tools.

## REST endpoints (new)

| Endpoint | Purpose |
| --- | --- |
| `GET /api/v1/repos/{o}/{n}/docs` | List doc metadata for a repo |
| `GET /api/v1/repos/{o}/{n}/docs/*path` | Serve one markdown doc |
| `GET /api/v1/docs/search?q=&repo=&top=` | RAG search → whole docs |
| `POST /api/v1/docs/ask` | (optional) RAG + LLM answer |

UI consumes these for rendering + search.

## UI (later pass)

- Docs browser: tree of externally stored generated docs per repo, markdown render.
- Search box → `/api/v1/docs/search`, show ranked docs, open full doc.
- Existing Svelte app in `_ui/` (build output in `internal/server/dist`) is the host.

## Build / rebuild semantics

- Docs regenerate only for files whose source hash changed (manifest in
  `docs-index.json`) to keep LLM cost bounded.
- On startup and before docs access/generation, legacy
  `<clone>/krabby-docs/` trees are moved to `data_dir/docs/<owner>/<repo>/`.
  This preserves manifests and summary caches, so migration makes no LLM calls.
- Vector index upsert is per-repo idempotent; `DeleteRepo` + re-`Upsert` on
  full rebuild, or per-doc upsert on incremental.
- Embedded store persists to disk on change; loads on startup.

## Failure & cost guards

- docgen and rag are **best-effort**: a missing/misconfigured LLM or embedder
  disables the feature with a warning, never breaks graph builds.
- `docs.enabled` / `rag.enabled` default behavior: enabled only when the
  respective `base_url`/credentials are configured.
- Concurrency + batching bound LLM/embedder load.

## Runtime configuration (settings store + live rebuild)

Docs/RAG must be configurable at runtime via MCP tools and the UI, not only via
`krabby.yaml`/env. File/env config becomes the **seed**; a persisted settings
record overrides it and can be changed live.

### `internal/service/settings`

- `Settings` struct = the docs/RAG configuration (docs enable/globs/concurrency,
  llm, embedder, rag). Mirrors the config blocks but is **mutable** and
  **persisted** in the existing `bw` DB (new `settings` bucket, single row).
- **Secrets are write-only**: `LLM.APIKey` and `Embedder.APIKey` carry `bw`
  persistence but `json:"-"`. A redacted view
  (`Redacted()`) exposes only `*_key_set` booleans. On update, an empty secret
  means "keep existing" (so the UI never has to re-send secrets).
- `Store.Get(ctx)` returns the current settings (seeded from file config on
  first run). `Store.Set(ctx, patch)` merges a patch (empty secret = keep) and
  persists.

### Manager provider (live rebuild)

The manager no longer holds `docgen`/`rag` directly. It holds a
`docsProvider` guarded by an `RWMutex`:

```go
type docsBundle struct {           // immutable snapshot; swapped atomically
    gen   docgen.Generator         // nil when docs disabled/unconfigured
    rag   *rag.Service             // nil when rag disabled/unconfigured
    store vectorstore.Store        // owned; closed on swap
}
```

- `Manager.Configure(ctx, s settings.Settings) error` builds a new `docsBundle`
  from `s` (llm→docgen, embedder+store→rag), then swaps it in under the write
  lock and closes the previous bundle's store. Called at startup (from the
  persisted/seeded settings) and on every settings update.
- Read paths (`SearchDocs`, pipeline hook, `RemoveRepo`) take the read lock and
  use the current bundle; a nil piece means that capability is off.
- Build failures (bad LLM/embedder/store) are returned to the caller (so the UI
  shows the error) **and** leave the previous working bundle in place.

### MCP tools

| Tool | Purpose |
| --- | --- |
| `get_docs_config` | Return the redacted current docs/RAG config (+ `*_key_set`). |
| `set_docs_config` | Update config (partial); empty secrets keep existing; rebuilds live. |

### REST

| Endpoint | Purpose |
| --- | --- |
| `GET /api/v1/docs/config` | Redacted current config |
| `PUT /api/v1/docs/config` | Update config, live rebuild, returns redacted result |

### UI

`Settings.svelte` gains a **Docs & RAG** section: a form for enable flags,
models, base URLs, chunking, and write-only secret
inputs (placeholder shows "set"/"not set"). Saving calls `PUT /docs/config` and
reflects the rebuild result/error inline.

### Precedence

file/env defaults → persisted settings (if present) → runtime `set_docs_config`.
The persisted row wins over file once written; a "reset to file defaults" action
deletes the row.

## Open questions

- Merged (cross-repo) docs: generate a merged overview, or only per-repo docs +
  cross-repo vector search? (Proposed: per-repo docs, cross-repo search via
  `repo=""`.)
- Chunker: markdown-aware (split on headings) vs fixed-size. (Proposed:
  heading-aware with size cap.)
- Do we want doc generation for non-Go repos in the first cut? (Proposed: yes,
  language-agnostic prompts driven by graph + file content.)
```
