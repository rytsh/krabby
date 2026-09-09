# Krabby details

[Back to README](README.md)

<img src="./_docs/public/krabby.webp" width="360" />

[![License](https://img.shields.io/github/license/rytsh/krabby?color=red&style=flat-square)](https://raw.githubusercontent.com/rytsh/krabby/main/LICENSE)
[![Coverage](https://img.shields.io/sonar/coverage/rytsh_krabby?logo=sonarcloud&server=https%3A%2F%2Fsonarcloud.io&style=flat-square)](https://sonarcloud.io/summary/overall?id=rytsh_krabby)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/rytsh/krabby/test.yml?branch=main&logo=github&style=flat-square&label=ci)](https://github.com/rytsh/krabby/actions)
[![Web](https://img.shields.io/badge/web-document-blueviolet?style=flat-square)](https://rytsh.github.io/krabby/)

Krabby provides code search, documentation retrieval, and relationship analysis
over MCP. Point it at repositories; it clones and indexes, docs and RAG them with LLM and Embeddings,
builds a [graphify](https://github.com/Graphify-Labs/graphify) knowledge graph per repo, and
keeps those indexes fresh in the background.

```
                     ┌───────────────────────────────────────┐
 LLM/Agent ──MCP───► │ krabby (Go)                           │
 (streamable HTTP)   │  ├─ MCP tools (manage + query)        │──► git clone/pull
                     │  ├─ REST API + provider-neutral hook  │──► graphify update
 CI/webhook ──HTTP─► │  ├─ Registry (bw/BadgerDB)            │──► graphify merge-graphs
                     │  ├─ Native graph query engine (Go)    │
                     │  └─ Scheduler (cron schedules)        │
                     └───────────────────────────────────────┘
```

- **Queries are fast**: the graph query tools (`query_graph`, `get_node`,
  `get_neighbors`, `get_community`, `god_nodes`, `graph_stats`, `shortest_path`)
  are answered **in-process by a native Go engine** that reads `graph.json`
  directly and hot-reloads it on rebuild — no per-graph subprocess is spawned.
- **Builds are cheap**: code extraction is AST-based — no LLM key needed. The
  graphify CLI is only invoked to build/merge graphs.
- **Docs & RAG (optional)**: with an LLM configured, krabby generates per-file
  Markdown documentation (prompt is configurable in Settings) plus a repo
  overview, browsable in the UI.
- **Web sources**: named Custom web URL collections and Confluence spaces are
  converted to Markdown and indexed beside repo docs. Search everything, all
  repos, all web sources, or one collection such as `web:wine`. Providers stream
  their items as they are fetched, so a 35k-ticket JIRA project or a whole
  Confluence space syncs in the same memory as a handful of pages.
- **API catalog**: OpenAPI/Swagger documents and gRPC servers are catalogued as
  groups of services, each service holding one endpoint per operation with its
  parameters, request/response schemas and a ready-to-run `curl`/`grpcurl`
  command. Endpoints are also indexed beside repo docs (`api:<name>`), so
  "which endpoint creates an invoice" is answerable by search and "how exactly
  do I call it" by a single lookup. Published specs are frequently wrong for the
  reader's environment, so a service can override the base URL, patch the raw
  document (RFC 7386 JSON Merge Patch, schemas included) and correct or hide
  individual endpoints.
- **Regex code search**: every indexed source chunk also carries a
  byte-trigram index in the same BadgerDB transaction as the chunk itself,
  so `search_code` answers RE2 patterns with exact file/line/column
  locations — signatures, punctuation, line anchors and case-sensitive
  lookups that a term index cannot express. See
  [Code search modes](#code-search-modes).
- **Semantic code search (optional)**: source is chunked at graphify symbol
  boundaries (with line-window fallback), embedded with a dedicated code model
  such as Codestral Embed, and returned as ranked path/line snippets.

## Website

The GitHub Pages site lives in `_docs/` and is deployed by GitHub Actions.
Run it locally with:

```sh
cd _docs
pnpm install
pnpm dev
```

## Requirements

- Go 1.26+ (build), git, ssh (for private repos)
- graphify CLI for building graphs: `uv tool install graphifyy==0.9.26`
  (or `pip install graphifyy==0.9.26`). This tested version is pinned in the
  container image; upgrade it together with the compatibility test. The MCP
  extra is **no longer required** — graph
  queries are answered in-process by krabby's native Go engine.

## Quick start

```sh
make build
cp krabby.example.yaml krabby.yaml   # edit repos, keys
./bin/krabby
```

Add a repo and query it:

```sh
curl -X POST localhost:8080/api/v1/repos -d '{"url":"git@git.example.com:team/service.git"}'
curl localhost:8080/api/v1/repos
```

MCP catalogs for agents (opencode, Claude Desktop, etc.) use streamable HTTP.
Krabby performs no authentication of its own on any endpoint — deploy it behind
a proxy that authenticates every request:

- `http://localhost:8080/mcp` — read-only repository, graph, file, history, and documentation tools
- `http://localhost:8080/mcp/api` — API discovery plus `call_api_endpoint`, which sends real requests
- `http://localhost:8080/mcp/admin` — every Krabby mutation and administrative tool

The catalogs are disjoint. Add them as separate MCP servers so a client can
enable API access or administration only for the tasks that require it.

Example OpenCode config with core and admin registered separately:

```json
{
  "mcp": {
    "krabby": {
      "type": "remote",
      "url": "http://localhost:8080/mcp"
    },
    "krabby-admin": {
      "type": "remote",
      "url": "http://localhost:8080/mcp/admin",
      "enabled": false
    }
  }
}
```

## MCP tools

| Tool | Purpose |
| --- | --- |
| `list_repos` / `repo_status` | Core: discover tracked repositories and inspect build state |
| `repo_overview` | Core: read a bounded introduction and section map from existing generated repo documentation, with generation and local-clone commit metadata; no model call |
| `add_repo` / `remove_repo` / `refresh_repo` | Admin: manage tracked repositories and rebuilds |
| `set_credential` / `list_credentials` / `remove_credential` | Admin: per-host / per-org git credentials |
| `search_code` | First choice for symbols, literals, usages and implementation locations; `mode` selects term (`normal`), regular-expression (`regex`) or vector (`semantic`) retrieval, and `path` narrows any of them to a file glob |
| `find_definition` / `find_references` | Locate a known symbol's definitions, or its usages as real graph edges, with a path and a line for each |
| `read_file` / `list_files` / `glob` | Page through a known source file, inspect one directory, or find files by path pattern. Browsing hides vendored trees; `glob` does not, because a caller who writes `vendor/**` asked for them |
| `query_graph` | Architecture, dependency, call/data-flow, and cross-file relationship questions |
| `get_node` / `get_neighbors` / `get_community` | Node-level inspection |
| `god_nodes` / `graph_stats` / `shortest_path` | Graph-level analysis |
| `list_refs` / `git_log` / `git_diff` / `git_blame` | Repository history: tags and branches, commits, what a revision changed, who last touched a line |
| `search_docs` / `list_docs` / `get_doc` | Search generated or synced Markdown, with evidence kind and source metadata; these are stored/indexed copies, not live Jira/Confluence queries |
| `list_namespaces` | Discover repository groups, counts, and human descriptions before broad search |
| `list_sources` / `get_source` | Discover web collections and their exact `web:<name>` scope keys; inspect bounded item-title samples |
| `register_source_page` / `import_source_pages` / `import_source_sitemap` / `delete_source_page` | Admin: manage individual `pages` source items |
| `source_types` / `get_source_config` / `add_source` / `update_source` / `delete_source` / `refresh_source` | Admin: manage Krabby collections and sync jobs (`pages`, `confluence`, `jira`), not upstream issues or pages |
| `queue_status` / `bump_task` / `cancel_task` / `set_task_concurrency` / `cancel_repo_job` | Admin: inspect and steer the background work queue |
| `set_repo_namespace` / `set_repo_overrides` / `set_namespace_description` / `delete_namespace` | Admin: namespaces and per-repository overrides |
| `list_api_groups` / `list_api_services` / `list_api_endpoints` / `get_api_endpoint` | API: walk the catalog from domain to service to endpoint to its full request shape |
| `call_api_endpoint` | API: send a real request to a catalogued HTTP or gRPC endpoint |
| `api_service_kinds` / `add_api_service` / `update_api_service` / `delete_api_service` / `refresh_api_service` / `get_api_service_config` | Admin: manage catalogued APIs |
| `set_api_group_description` / `delete_api_group` | Admin: manage API group descriptions |
| `get_docs_config` / `set_docs_config` | Admin: read or live-update docs and code RAG settings |
| `test_llm` / `test_embedder` / `test_code_embedder` | Admin: validate model endpoints without saving |

Always pass the full repo id (`host/group/.../name`) when it is known. Omit it
only for an intentional cross-repository search or merged-graph analysis.
`list_*` tools are for discovering unknown identifiers or explicit inventory
requests; responses are paginated and agents should not exhaust every page by
default. Source and document reads are also bounded and expose continuation
metadata for large files.
`repo_overview`, `get_doc`, `search_docs`, `list_sources`, and `list_namespaces` expose MCP output schemas
and structured content. Search hits identify `source_kind` and `scope_key`, plus
the repository namespace or web collection metadata, so clients do not need to
infer source identity from an overloaded id string.

With a dedicated Jira/Confluence MCP also enabled, use that provider for current
state, live queries and upstream changes. Use Krabby for indexed context and
connections to code, handing off the original result URL when live verification
is needed. `evidence.kind` distinguishes `generated_summary`, `synced_snapshot`
and `catalog_snapshot`. Collection refresh time and sync/item status describe
Krabby ingestion, not Jira workflow status or a guarantee of complete coverage.
An empty local result does not prove the upstream item is absent.

For repo orientation, call `repo_overview` with a known repo id. Its introduction
is capped at 4 KiB; up to 30 section offsets are extracted from the first 128 KiB
of the document, with explicit truncation flags. Pass a section's byte offset and
the returned document path to `get_doc`, then verify relevant files/symbols with
`search_code` and `read_file`. The overview reuses existing documentation even
without an embedder; `available:false` means no repository synthesis is available.
Generated docs describe selected, budget-limited inputs and are navigation aids,
not primary source evidence. New generations include source-file identities and
instructions for a concrete investigation map.

`read_file` and paginated `list_files` responses include a `snapshot` token;
pass it back on continuation calls so every page stays on the same immutable
repository version even if a refresh activates meanwhile. The token is a soft
hint, not a lock: it is honored only while that version is still retained (the
newest previous version plus a short grace window, 5m by default). Once the version is
reaped, replaying its token transparently falls back to the current active
snapshot and returns the new token — it never errors or wedges a client, so a
consumer that keeps replaying an old token simply advances to the latest.

Because each endpoint publishes only its own catalog, a client pays the
`tools/list` context cost only for the capabilities currently enabled.

## REST API

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness — unauthenticated, this is what a health check should target |
| `GET /api/v1/repos` | List repos |
| `POST /api/v1/repos` `{"url","branch"}` | Track a repo; an already tracked url just queues a refresh, so it takes the same optional `skip`. Overrides named in the body are applied to a tracked repo (ones left out are kept); `branch` and `namespace` are not |
| `GET /api/v1/repos/{full-path...}` | Repo status |
| `DELETE /api/v1/repos/{full-path...}` | Untrack + delete clone |
| `POST /api/v1/repos/{full-path...}/-/refresh` | "This repo changed" trigger; optional `{"skip":["docs"]}` drops stages for that run |
| `GET /api/v1/repos/{full-path...}/-/graph` | Raw `graph.json` of one repo |
| `GET /api/v1/repos/{full-path...}/-/report` | `GRAPH_REPORT.md` audit report |
| `GET /api/v1/repos/{full-path...}/-/html` | Interactive graph visualization |
| `GET /api/v1/graph` | Merged cross-repo `graph.json` |
| `GET/POST /api/v1/sources` | List/create named Custom web or Confluence collections |
| `GET/PUT/DELETE /api/v1/sources/{name}` | Read/update/delete a collection |
| `POST /api/v1/sources/{name}/refresh` | Sync and reindex a collection |
| `POST/DELETE /api/v1/sources/{name}/pages` | Add/remove Custom web URLs |
| `POST /api/v1/sources/{name}/pages/import` | Import HTML/Markdown pages, including URL-less manually authored Markdown, and index them |
| `POST /api/v1/sources/{name}/sitemap` | Import URLs from a Custom web sitemap |
| `GET/POST /api/v1/apis/groups` | List/describe API catalog groups |
| `DELETE /api/v1/apis/groups/{name}` | Delete a group description (services keep their tag) |
| `GET /api/v1/apis/kinds` | List the registered service kinds (`openapi`, `grpc`) |
| `GET/POST /api/v1/apis/services` | List/create catalogued API services |
| `POST /api/v1/apis/services/config/test` | Probe a document or gRPC target without saving |
| `GET/PUT/DELETE /api/v1/apis/services/{name}` | Read one service with a page of its endpoints, update, or delete |
| `POST /api/v1/apis/services/{name}/refresh` | Re-fetch and reindex; `?force=true` re-renders from an unchanged document |
| `GET /api/v1/apis/services/{name}/operation?id=` | One endpoint's full structured detail |
| `GET /api/v1/browser-extension.zip` | Download the browser importer matched to the running Krabby version |
| `GET /api/v1/docs/search?q=&mode=&scope=&repo=&top=` | Docs search; `mode=hybrid|semantic|lexical`, `scope=all|repos|sources|apis`, `repo=web:<name>` or `api:<name>` |
| `GET /api/v1/code/search?q=&mode=&repo=&namespace=&path=&page=&per_page=&top=` | Source-code search; `mode=normal` (BM25, default), `mode=regex` (RE2, extra `case_sensitive`, `context_lines`, `max_matches`) or `mode=semantic` (vectors, `top`); `path` is a file glob applied to every mode |
| `GET /api/v1/repos/{full-path...}/-/glob?pattern=&limit=&snapshot=` | Find files in a clone by path pattern |
| `GET/PUT /api/v1/docs/config` | Read/update docs and code RAG settings |
| `GET /api/v1/credentials` | List credential patterns (secrets never returned) |
| `PUT /api/v1/credentials` `{"pattern","secret","kind","username"}` | Store a credential |
| `DELETE /api/v1/credentials?pattern=...` | Remove a credential |
| `POST /webhook/git` | Provider-neutral git push webhook; generic bearer/shared-token auth plus common server formats |

### Health checks and the MCP endpoint

Point liveness and readiness probes at **`/healthz`** (or `<base_path>/healthz`).
It is unauthenticated, cheap and answers `200 OK`.

The MCP endpoint is not a health check endpoint, but it tolerates being used as
one. The Streamable HTTP transport speaks JSON-RPC over `POST` and reserves
`GET` for opening the SSE stream of an established session, so every ordinary
probe violates its preconditions and used to be answered `400`:

| Probe | Before | Now |
| --- | --- | --- |
| `GET`, no `Accept` (Kubernetes `httpGet`, Go's `http.Get`) | `400 Accept must contain 'text/event-stream'` | `200` |
| `GET`, `Accept: */*` (`curl`, `wget`) | `400 GET requires an Mcp-Session-Id header` | `200` |
| `HEAD` | `400` | `200` |
| Browser navigation | `400` | `200` |

Those `400`s were correct per the transport spec and useless as a signal: a
prober cannot distinguish "your request was malformed" from "this server is
broken". `GET` and `HEAD` that carry no `Mcp-Session-Id` header are now answered
with a small JSON descriptor instead. Nothing that a client uses is affected —
`POST`, `DELETE`, and any `GET` carrying a session id go straight to the
transport — because a request without that header could only ever have received
an error.

## Code search modes

`search_code` (and `GET /api/v1/code/search`) exposes three retrievals over
the same indexed chunks. They answer different questions and none of them
subsumes another:

| `mode` | Engine | Ask it for | Returns |
| --- | --- | --- | --- |
| `normal` (default) | BM25 over path, symbol and chunk text | an identifier, a literal, or a question in words | ranked chunks with a best-guess line |
| `regex` | RE2 verified over trigram-selected chunks | a signature, punctuation, an anchored line, a case-sensitive name | files with exact line/column locations and optional context |
| `semantic` | embeddings + HNSW (needs a code embedder) | a concept you cannot spell as a token | ranked chunks by cosine similarity |

All three take `path`, a glob over the repo-relative source path. Without a
slash it matches a file name at any depth (`*_test.go`); with one it is
anchored at the repository root (`internal/**`). A trailing slash is the
subtree shorthand (`internal/` means `internal/**`) and `..` is refused as a
path segment. It is a structured argument rather than a `path:` token inside
the query, so it cannot be mis-escaped and reads the same from an MCP client
and from the UI's form. The `glob` tool normalizes a pattern with the same
code, so one pattern means one thing in a search and in a file lookup.

**`normal` takes a question.** bw ANDs a bare query's terms, so passing a
sentence verbatim used to require all of its words inside one 3000-character
chunk and reliably returned nothing. The query is rewritten first: prose words
become an OR chain that BM25 ranks, while identifier-shaped words — anything
containing a digit or one of `. - _ / + @`, so `bw.ErrNotFound` or `PAY-1842` —
stay required. An exact lookup therefore keeps its precision while a question
starts working. Operators are passed through untouched, so `"a phrase"`,
`a OR b`, `-excluded` and `prefix*` remain available.

Terms the indexed source itself shows to be ubiquitous are dropped from the OR
chain. A source corpus is full of `func`, `return` and `error`: each costs a
posting-list walk proportional to the whole index while contributing almost
nothing to the ranking. The set is derived from the corpus by sampling, not
from a word list, so it needs no knowledge of the language and adapts to the
domain — the same mechanism the documentation index uses, sharing its
threshold. Filtering is an optimisation and never allowed to cost a result: a
filtered query that comes back empty is retried unfiltered.

**Scoping is one key filter, not a fan-out.** Everything a scope needs is in
the chunk id (`<repo>/<path>#<n>`), so the repository set and the path glob are
string tests on a key the ranking already produced — a function call per hit
instead of a point read and a decode. A namespace search is therefore a single
pass whose `total` is a real count; it used to fan out per repository and merge
the pages, which made the total a merged candidate count and left scores
unranked across repositories. bw's corpus statistics are index-wide, so the
scores were comparable all along.

**Every page reports its index state.** `indexed[]` carries, per repository
behind the page, the commit the index was built from, when it finished, and
whether the clone has since moved past it (`stale`). A hit is only as current
as the index it came from and nothing else in the result says so. When nothing
matched, the repositories the search *looked at* are reported instead, because
"no matches" and "nothing indexed yet" are answers a caller has to be able to
tell apart.

**`regex` is exact and located.** The pattern is RE2 (Go's `regexp`), matched
in multi-line mode, so `^` and `$` are line anchors as they are in grep and
`.` still stops at a newline. Matches are grouped per file and each carries
the file line, the byte column within that line, the matching line's text
and — with `context_lines` — the lines around it. Context comes from the
chunk the match landed in, which is a symbol-sized window rather than the
whole file; `read_file` is still the way to see more.

`total` counts matching **files** and is exact, unless `exhaustive` is
`false` — which means the search stopped at its file ceiling and there are
more. Case is off by default (`case_sensitive`), matches per file are capped
(`max_matches`, default 20) and a file that hit the cap says
`truncated: true` rather than quietly dropping occurrences.

### How regex search is answered

Every source chunk carries a byte-trigram index in the same BadgerDB
transaction as the chunk itself (`bw`'s `trigram` field tag), so the index is
never out of step with the content and a backup carries it along.

A pattern is planned into a boolean expression over trigrams that any match
must satisfy — `errors\.Is\(` requires the trigrams of that literal run —
and only the chunks satisfying it are read and matched. What the planner
cannot constrain it widens to "no constraint": `[a-z]+` or `ab.cd` has no
three-byte literal run, so those searches scan the scoped chunks instead.
They are slower and return the same answer.

Matching happens inside a chunk, which is visible twice: a match that straddles
a chunk boundary is not found, and `context_lines` stop at the chunk's edge. A
single source line longer than twice `coderag.chunk_size` is also indexed only
up to that cap - the ceiling that keeps a minified bundle from filling the
embedder's context window applies to every mode.

A `repo` that names nothing tracked is an error in all three modes, not an
empty page. An empty page from a code search reads as "this code does not
exist", so a mistyped id must never produce one.

One consequence worth knowing: **the index costs disk.** Trigram postings add
roughly 3-4× the indexed source size to `state/`, driven by chunk granularity —
a larger `coderag.chunk_size` amortises each posting over more source bytes.
The index is built by the same `code_index` stage as BM25, from the same
chunks, so it needs no extra pass over the clone and no model.

Upgrading an existing install backfills it: the `code_search` bucket moves
to schema v2 and every stored chunk is rewritten through the normal write
path once, which is what emits its trigram postings. Nothing is re-chunked
and no repository is re-read.

## Symbol navigation

`find_definition` and `find_references` answer "where is this defined" and
"where is this used" from the knowledge graph rather than from text.

That distinction is the point. A search-based implementation — the usual one —
resolves a definition by looking for the symbol's name in a symbol index and
its references by matching the name as a word, which counts comments, string
literals and unrelated identifiers that happen to share it. Krabby already has
the structure to do better: a graph node carries its `source_file` and
`source_location`, and an edge carries the `context` in which one node used
another (`call`, `import`, `field`, `parameter_type`, `return_type`,
`generic_arg`, `attribute`, `export`). A definition is therefore a node and a
reference is an incoming edge, each with a path and a line.

- **Every definition, not a pick.** A symbol defined in three files returns
  three definitions. Resolution is exact label, then prefix, then substring;
  matches from weaker tiers are reported as `candidates` rather than mixed in,
  so an ambiguous symbol is visible instead of silently narrowed.
- **File, concept and JSON-key nodes are excluded**, since a `.json` noise key
  is not a definition — and `note` says so when the exclusion is what emptied
  the result.
- **`context` narrows the question.** `context:['call']` answers "who calls
  this" rather than "who mentions it". A filter matching none of the symbol's
  actual edge contexts returns the vocabulary it does have, because the
  dangerous failure here is concluding a function has no callers from a filter
  that matched nothing.
- **A symbol the graph does not know returns a `note`** naming `search_code`
  with `mode='regex'` and a word-boundary pattern, not an empty list. There is
  deliberately no text fallback inside the graph layer: the caller decides
  whether to accept a text match in place of a structural one.

The limit is the extractor's coverage. graphify is AST-based and needs no
model, but a language it does not parse has no nodes, and locals and
parameters have no nodes in any language — for those, regex search is the
right tool and the `note` points at it.

## Data layout & external tools

Everything lives under `data_dir` (default `~/.krabby`) and is plain files —
other tools (doc generators, linters, indexers) are free to read it:

```
~/.krabby/
├── repos/.snapshots/<host>/<group>/.../  # immutable versioned git snapshots
│   └── <version>/graphify-out/
│       ├── graph.json          # raw graph (GraphRAG-ready)
│       ├── GRAPH_REPORT.md     # human-readable audit report
│       ├── graph.html          # interactive visualization
│       └── manifest.json       # incremental-update manifest
├── merged/graph.json           # cross-repo merged graph
├── keys/                       # materialized credential SSH keys (0600)
├── docs-vectors/               # embedded documentation vector index
├── sources/<name>/*.md         # synced Custom web / Confluence pages
├── apis/<name>/*.md            # API catalog endpoint projections (one per operation)
├── code-vectors/               # embedded source-code vector index
└── state/                      # registry + credentials database
```

`GET /api/v1/repos` returns each repo's local `path` for discovery, and the
artifact endpoints above serve the same files over HTTP for tools that have no
filesystem access.

### Immutable repository snapshots

Refreshes fetch remote state without changing the active working tree. Krabby
clones and rebuilds the graph in a private version directory, then publishes the
source and graph together by atomically replacing the registry path. Failed or
cancelled builds leave the previous version active. The newest previous snapshot
is retained, and older versions receive a short grace period (5m by default) for
readers that resolved an earlier path; a read replaying a reaped snapshot token
transparently falls back to the current version instead of failing.

## Git credentials

Credentials are stored per **pattern** — a host or host/path prefix — and the
most specific match wins when cloning/pulling:

```sh
# SSH key for a whole GitLab instance (kind inferred from the PEM):
curl -X PUT localhost:8080/api/v1/credentials \
  -d '{"pattern":"gitlab.example.com","secret":"-----BEGIN OPENSSH PRIVATE KEY-----\n..."}'

# Token for one organization (https clones):
curl -X PUT localhost:8080/api/v1/credentials \
  -d '{"pattern":"git.example.com/myorg","secret":"token..."}'
```

Or let the LLM do it over MCP with `set_credential`. SSH keys are materialized
under `data_dir/keys/` with 0600 perms; tokens are fed to git via a credential
helper (never on argv). Secrets are never returned by any API. The global
Git credentials are persisted by host or host/path pattern through the UI,
REST API or MCP tools. The most specific pattern wins.

## API catalog

An API service is one OpenAPI/Swagger document fetched over HTTP, or one gRPC
server enumerated through server reflection. Services are filed into groups, and
each group carries a description — that description is what an agent reads to
decide where to look, so it is worth writing.

Reading the catalog is deliberately a walk, not a dump:

```
list_api_groups     which area of the estate      (tens of tokens)
list_api_services   which API in that area        (tens per service)
list_api_endpoints  which endpoint in that API    (~20 per endpoint)
get_api_endpoint    exactly how to call it        (bounded, ~1-3k)
```

A single specification routinely runs to hundreds of kilobytes, so handing one
to a model is both unaffordable and useless. Only the last step returns schemas,
and what it returns is pre-rendered at sync time: `$ref` resolution, `allOf`
merging and schema flattening happen once per refresh, not once per question.
Schemas are capped at 4 levels deep and 40 properties per object; anything cut
is reported as `truncated` rather than silently dropped. Recursive types are
stubbed by name at their first repeat.

Every endpoint is also projected to Markdown under `apis/<name>/` and indexed
into the docs RAG as `api:<name>`, so `search_docs` answers "which endpoint
creates an invoice" and `get_api_endpoint` answers "how exactly do I call it".

### Overriding a specification

Published documents are frequently wrong for the reader's environment: they name
a public host the caller cannot reach, their descriptions are thin, or a schema
does not match what was actually deployed. Rather than requiring the document to
be fixed at its source, a service carries three override layers, applied from
most general to most specific so a targeted fix is never undone by a broad one:

| Layer | Field | What it reaches |
| --- | --- | --- |
| 1 | `spec_patch` | The raw document, before parsing. An RFC 7386 JSON Merge Patch, so it can change **anything**, schemas included; a `null` value deletes a key. |
| 2 | `base_url` | Replaces whatever servers the document declares, and therefore every generated request. |
| 3 | `operations` | Per-endpoint `summary`, `description`, `tags` and `hidden`, keyed by operation id or `"METHOD /path"`. |

`hidden` is the escape hatch for documents that publish internal or deprecated
endpoints which would otherwise be offered to a model as valid choices.

Changing any override forces a full re-sync. The document has not changed, but
what krabby renders from it has, and conditional fetching would otherwise leave
the edit invisible until the upstream specification happened to move.

```jsonc
{
  "name": "billing",
  "kind": "openapi",
  "group": "finance",
  "description": "Invoicing, payments and dunning.",
  "base_url": "https://billing.internal.corp",
  "config": { "url": "https://docs.corp/billing/openapi.yaml", "token": "..." },
  "spec_patch": {
    "components": { "schemas": { "Money": { "properties": {
      "amount": { "type": "string", "description": "Minor units, as a decimal string." }
    } } } }
  },
  "operations": {
    "deleteAllInvoices": { "hidden": true },
    "POST /v1/invoices": { "summary": "Raise a new invoice" }
  },
  "specs": ["0 3 * * *"]
}
```

Secrets (`token`) are write-only: they are never returned by any API, and a
blank value on update keeps the stored one. Syncs are conditional — an `ETag`,
a `Last-Modified`, or a hash of the fetched bytes — so a service polled hourly
against a static document is not re-embedded twenty-four times a day.

Creating a service points krabby at a URL or a gRPC target of the operator's
choosing, including private addresses; that is the intended use, and the
security boundary is who can access the admin MCP endpoint, not the network.

## Refresh pipeline

```
webhook / poll / refresh_repo
  → git fetch (new commits?) → git pull
  → graphify update <repo>          # incremental, AST-only, no LLM
  → graphify merge-graphs → merged/graph.json
  → code RAG index (when enabled)
  → generated docs + docs RAG index (when enabled)
```

Stages can be dropped in two ways. A repository's `skip_stages` override turns
a stage off permanently (`set_repo_overrides`, or the checkboxes on the repo
page). For a one-off — a small code change that cannot affect the
documentation — pass `skip` to `refresh_repo`, to `POST .../-/refresh` or to
`POST /api/v1/repos` instead: it applies to that run only and leaves the
override alone. Skipping
`docs` also skips `docs_index`, which would otherwise re-embed the previous
run's Markdown at full cost. A run that skips the graph still promotes the new
clone, so the next unskipped refresh rebuilds the graph even at the same commit.

Krabby records the Graphify version beside each validated graph. On startup it
queues a one-time rebuild for graphs produced by another version (or predating
the marker), so extractor upgrades also refresh repositories with no new commit.

Repos are also polled on cron schedules configured in Settings. Each schedule
targets a namespace (`*` = all, `default` = untagged) and carries one or more
cron specs (e.g. `0 */6 * * *`, or `@every 15m`), so different namespaces can
poll on different cadences and multiple schedules may coexist. With no schedule
configured, polling falls back to a fixed interval (default 1h). Cron scheduling
uses [worldline-go/hardloop](https://github.com/worldline-go/hardloop); note
day-of-month and day-of-week are combined with AND. Changes apply without
restarting krabby.

## Configuration

See [krabby.example.yaml](krabby.example.yaml). Loaded via
[chu](https://github.com/rakunlabs/chu): defaults → `krabby.yaml` (or
`CONFIG_FILE`) → `KRABBY_*` env vars.

Docs RAG and code RAG are independently switchable in the Settings UI. Code RAG
can use its own embedder; when unset it reuses the docs embedder.
Optional web-image analysis is also configured globally in Settings and is off
by default. It is bounded to 3 images per page, 4 MiB per image and about 16
megapixels; authenticated and same-origin private-network images require a
separate opt-in. Enabling analysis may send private image bytes to the
configured vision provider. Each source
must additionally enable **Analyze images in this source**. Results are cached
by image content hash and vision model; raw image bytes are not persisted.
Documentation indexes omit Markdown link destinations and image source URLs by
default while retaining link labels and image alt text. Enable **Keep link and
image URLs in search indexes** under Settings → Retrieval when URL terms should
also participate in lexical and semantic search. Saving either mode rebuilds the
derived indexes; the stored Markdown is unchanged.
The embedded backend keeps docs and code in separate stores so different vector
dimensions are safe.
Generated markdown is stored outside clones under
`data_dir/docs/<owner>/<repo>/`; older in-clone `krabby-docs/` trees are moved
there at startup without regenerating documentation.

### LLM observability (Langfuse)

Every model call krabby makes can be exported to
[Langfuse](https://langfuse.com) as a trace: model, latency, time to first
token, prompt and completion tokens, and the cost Langfuse derives from them.
Configure it under **Settings → LLM observability**, or through
`set_docs_config` / `PUT /api/v1/docs/config`. Nothing is exported until a host
and a project key pair are supplied.

Traces are posted to `<host>/api/public/otel/v1/traces` — the signal-specific
path, not the `/api/public/otel` base endpoint. The base form is what
`OTEL_EXPORTER_OTLP_ENDPOINT` and the OTel Collector take, because they append
`/v1/traces` themselves; a client that configures the path directly must spell
it out, and posting to the base form returns `404`. **Test connection** probes
that exact URL, so a Langfuse older than v3.22.0 — which serves the REST API but
has no OTLP endpoint — is reported rather than silently dropping every export.

The export deliberately does **not** go through the `telemetry` collector. Two
reasons:

- Langfuse ingests OTLP over HTTP only — it does not accept gRPC — while
  [tell](https://github.com/rakunlabs/tell) builds its exporter on
  `otlptracegrpc`. krabby therefore runs a second, dedicated tracer provider
  for Langfuse.
- Langfuse bills per observation. The global provider carries an HTTP server
  span for every REST call and UI poll; none of that belongs in an
  LLM-observability backend.

The two providers are independent and can run at the same time.

**Trace shape.** One documentation build is one trace, sessioned by repository
id so every build of a repository lines up in the Langfuse UI. Under it sits one
generation per summary group plus the final synthesis. MCP tool calls are their
own traces, which is what shows an agent's actual behaviour. Retries are folded
into the observation they belong to rather than emitted as siblings, so a
rate-limited build does not triple its observation count.

**Scopes** are switched independently:

- **Documentation LLM calls** — the summary and synthesis chat calls.
- **Embedding calls** — one observation per `Embed` call rather than per batch;
  indexing a large repository issues thousands of batches.
- **MCP tool calls** — what a connected agent actually asked for.
- **REST API requests** — wraps each `/api/v1` call so a search made from the UI
  shows the embedding it caused nested underneath it, rather than as a
  standalone observation. Scoped to the API group only: `/healthz`, the pprof
  endpoints, the embedded UI assets and the MCP path are excluded, because a
  liveness probe every ten seconds is 8.6k observations a day of nothing. Off by
  default — the UI polls, so most of what it adds is requests that did no model
  work.

A span attaches to whichever krabby unit of work is above it and starts its own
trace when there is none. That distinction is explicit rather than inherited
from OpenTelemetry: with the `telemetry` collector configured, the HTTP
middleware leaves a span from the *other* provider on the request context, and a
generation that parented to it would carry a parent id Langfuse never receives —
which Langfuse drops, since it does not materialise a trace without its root.

**Content capture** is the setting that deserves thought, and the UI states this
next to the control:

- `full` (the default) sends prompts and replies whole. Summary prompts embed
  the files being documented, so a private repository's source is transmitted to
  the configured instance verbatim — on Langfuse Cloud, to a third party.
- The payload is not small. A synthesis prompt reaches 256 KiB and a summary
  prompt 96 KiB, so a forty-group build exports a few megabytes. The exporter is
  therefore configured with an export batch of 8 spans and a queue of 256, far
  below the OpenTelemetry defaults (512/2048), which would either produce a
  request Langfuse rejects or hold more pending spans than the memory budget
  allows. A build busy enough to fill that queue drops spans rather than growing.
- `max_content_bytes` caps a single captured value in every mode, `full`
  included, and defaults to 1 MiB. Setting it to `0` removes the cap and is only
  safe against a self-hosted instance with a matching body limit.
- `truncated` clips to 8 KiB and keeps prompts debuggable; `off` exports only
  model, latency, tokens and cost.

Observability changes rebuild the LLM/embedder clients (the tracer is attached
to them) but touch nothing that went into a vector, so — unlike an ordinary
settings save — they do not reindex. The exporter itself is reused across
unrelated settings changes: tearing it down mid-build would drop whatever it had
not yet shipped.

Token accounting is captured independently of Langfuse: krabby requests
`stream_options.include_usage` and reads the usage chunk. Gateways that reject
the parameter are detected on the first 400 that names it, and the request is
replayed without it — usage reporting is best-effort and never costs a
completion.

### Memory

krabby sizes itself from one number: the `memory.limit_bytes` setting, or — when
it is unset, which is the norm — the cgroup limit of the container it runs in,
falling back to total system memory. From that budget it derives Go's soft heap
limit (`GOMEMLIMIT`), the block/index caches and memtable size of each embedded
database, and the parsed-graph cache. Run the container with an explicit memory
limit (`docker run -m 4g`, or `resources.limits.memory`) so it has a real number
to work from. An explicit `GOMEMLIMIT` in the environment always wins.

Three things about the design are worth knowing when tuning it:

- **Memtables dominate restart cost.** Badger allocates a full arena per
  write-ahead log it finds at startup, and a kill (including an OOM kill) leaves
  those logs unflushed — so an under-sized budget makes the next start more
  expensive than the last. Krabby drains logs written under an older, larger
  memtable size before applying a smaller one, because replaying such a log into
  a smaller arena aborts the process outright.
- **Indexing is streaming.** Chunking, embedding and upserting happen in fixed
  batches, so peak memory does not grow with the size of a repository or a
  synced JIRA/Confluence collection. The trade-off is that a failure part-way
  through a full rebuild leaves a partial index rather than the previous one;
  the next refresh repairs it.
- **Fetching is streaming too, and that is a correctness property.** A web
  source provider hands each page to the sync as it converts it, so the walk
  costs the same memory at any size. That is what lets a full sweep run to
  completion, and only a sweep that completes may report itself as the
  collection's whole inventory — which is the sole licence the sync has to
  delete a stored page it did not see. A run cut short by `max_issues` /
  `max_pages` says so, and the sync then prunes nothing beyond what the provider
  explicitly reported as removed. Both caps default to unlimited; setting one
  bounds a single run's time and API spend and, as a consequence, suspends
  deletion reconciliation.
- **The startup warm passes are sequential.** Backfilling the code and docs
  search indexes runs one after the other in the background, so a restart never
  pays for both at once.
- **Write batches size themselves.** Badger caps a transaction at 15% of the
  memtable, so tuning the memtable for memory also changes how much can be
  written at once — and how much a batch costs depends on the embedding model's
  width, the document's vocabulary and, for vectors, how large the HNSW graph
  already is. None of that is knowable in advance, so the write paths start
  optimistic and halve on `ErrTxnTooBig`, keeping the size they find.

The vector cache deserves a note because it is the one bound `GOMEMLIMIT`
cannot enforce: its contents are live, so the collector can only thrash against
them. The embedded vector store ([bw](https://github.com/rakunlabs/bw)) keeps
decoded embeddings in memory to serve HNSW traversal, and their cost is decided
by the embedding model — the same number of vectors is ~75 MiB at 96 dimensions
and ~1.2 GiB at 1536. krabby therefore gives bw a **byte** budget derived from
the process limit (`bw.WithVectorCacheBytes`, bw v0.3.7+) rather than letting a
vector count stand in for one. A narrower embedding model buys a larger working
set for the same memory; a wider one buys better recall for less.

With a Matryoshka-trained model that trade is a setting rather than a change of
model: the embedding dim in Settings is sent to the provider as the OpenAI
`dimensions` parameter, so Gemini Embedding 2 (128–3072) or text-embedding-3 can
be held at a fraction of its default width with little accuracy lost and a
proportional cut in vector memory. Providers that do not accept the parameter
are detected on the first request and stay at their native width — `Test
embedder` reports the width actually returned, not the one asked for. Changing
the dim changes the index dimension, which wipes and rebuilds it.

`GET <base>/debug/pprof/heap` is available for when the numbers still do not add
up, but it is off by default — set `server.pprof: true` to mount it. The
profiling handlers are unauthenticated like every other route, and a heap dump
is a copy of process memory, which holds git tokens, LLM API keys and repository
content. Turn it on for an investigation and turn it back off afterwards.
