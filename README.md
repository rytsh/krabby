<img src="./assets/krabby.webp" width="360" />

Krabby provides code search, documentation retrieval, and relationship analysis
over MCP. Point it at repositories; it clones and indexes them, builds a
[graphify](https://github.com/safishamsi/graphify) knowledge graph per repo, and
keeps those indexes fresh in the background.

```
                     ┌───────────────────────────────────────┐
 LLM/Agent ──MCP───► │ krabby (Go)                           │
 (streamable HTTP)   │  ├─ MCP tools (manage + query)        │──► git clone/pull
                     │  ├─ REST API + provider-neutral hook  │──► graphify update
 CI/webhook ──HTTP─► │  ├─ Registry (bw/BadgerDB)            │──► graphify merge-graphs
                     │  ├─ Native graph query engine (Go)    │
                     │  └─ Scheduler (poll interval)         │
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
  repos, all web sources, or one collection such as `web:wine`.
- **Semantic code search (optional)**: source is chunked at graphify symbol
  boundaries (with line-window fallback), embedded with a dedicated code model
  such as Codestral Embed, and returned as ranked path/line snippets.

## Requirements

- Go 1.26+ (build), git, ssh (for private repos)
- graphify CLI for building graphs: `uv tool install graphifyy`
  (or `pip install graphifyy`). The MCP extra is **no longer required** — graph
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

MCP endpoint for agents (opencode, Claude Desktop, etc.): `http://localhost:8080/mcp`
(streamable HTTP; set `mcp.api_key` to require `X-Api-Key` / `Authorization: Bearer`).
Without a profile header it exposes the 20-tool standard catalog. Send
`X-Krabby-Tool-Profile: full` on every MCP request to additionally expose
credentials, clone leases, docs/RAG configuration, and endpoint probes.

Example opencode config:

```json
{
  "mcp": {
    "krabby": {
      "type": "remote",
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

For the full profile, add a persistent header to the same server entry:

```json
"headers": { "X-Krabby-Tool-Profile": "full" }
```

## MCP tools

| Tool | Purpose |
| --- | --- |
| `list_repos` / `add_repo` / `remove_repo` | Manage tracked repositories |
| `refresh_repo` | Pull + rebuild graph in the background |
| `repo_status` | Build state, last commit, last error |
| `set_credential` / `list_credentials` / `remove_credential` | Per-host / per-org git credentials |
| `lock_repo` / `unlock_repo` | TTL-bounded read locks for external consumers |
| `search_code` | First choice for symbols, paths, literals, definitions, usages, and implementation locations |
| `read_file` / `list_files` | Page through a known source file or inspect a bounded directory listing |
| `query_graph` | Architecture, dependency, call/data-flow, and cross-file relationship questions |
| `get_node` / `get_neighbors` / `get_community` | Node-level inspection |
| `god_nodes` / `graph_stats` / `shortest_path` | Graph-level analysis |
| `search_docs` / `list_docs` / `get_doc` | Search bounded excerpts and page through generated or synced Markdown |
| `list_sources` | Discover named Custom web and Confluence collections (`web:<name>`) |
| `get_docs_config` / `set_docs_config` | Read or live-update docs and code RAG settings |
| `test_llm` / `test_embedder` / `test_code_embedder` | Validate model endpoints without saving |

Always pass the full repo id (`host/group/.../name`) when it is known. Omit it
only for an intentional cross-repository search or merged-graph analysis.
`list_*` tools are for discovering unknown identifiers or explicit inventory
requests; responses are paginated and agents should not exhaust every page by
default. Source and document reads are also bounded and expose continuation
metadata for large files.

The `standard` profile omits the credential, lease, and docs/RAG administration
rows above. Configure `X-Krabby-Tool-Profile: full` when an MCP client must
administer them.

## REST API

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness |
| `GET /api/v1/repos` | List repos |
| `POST /api/v1/repos` `{"url","branch"}` | Track a repo |
| `GET /api/v1/repos/{full-path...}` | Repo status |
| `DELETE /api/v1/repos/{full-path...}` | Untrack + delete clone |
| `POST /api/v1/repos/{full-path...}/-/refresh` | "This repo changed" trigger |
| `POST /api/v1/repos/{full-path...}/-/lock` `{"owner","ttl"}` | Take a read lock (returns token) |
| `GET /api/v1/repos/{full-path...}/-/lock` | Lock status |
| `DELETE /api/v1/repos/{full-path...}/-/lock` + `X-Lock-Token` | Release the lock |
| `GET /api/v1/repos/{full-path...}/-/graph` | Raw `graph.json` of one repo |
| `GET /api/v1/repos/{full-path...}/-/report` | `GRAPH_REPORT.md` audit report |
| `GET /api/v1/repos/{full-path...}/-/html` | Interactive graph visualization |
| `GET /api/v1/graph` | Merged cross-repo `graph.json` |
| `GET/POST /api/v1/sources` | List/create named Custom web or Confluence collections |
| `GET/PUT/DELETE /api/v1/sources/{name}` | Read/update/delete a collection |
| `POST /api/v1/sources/{name}/refresh` | Sync and reindex a collection |
| `POST/DELETE /api/v1/sources/{name}/pages` | Add/remove Custom web URLs |
| `GET /api/v1/docs/search?q=&scope=&repo=&top=` | Semantic docs search; `scope=all|repos|sources`, `repo=web:<name>` |
| `GET /api/v1/code/search?q=&repo=&top=` | Semantic source-code snippet search |
| `GET/PUT /api/v1/docs/config` | Read/update docs and code RAG settings |
| `GET /api/v1/credentials` | List credential patterns (secrets never returned) |
| `PUT /api/v1/credentials` `{"pattern","secret","kind","username"}` | Store a credential |
| `DELETE /api/v1/credentials?pattern=...` | Remove a credential |
| `POST /webhook/git` | Provider-neutral git push webhook; generic bearer/shared-token auth plus common server formats |

## Data layout & external tools

Everything lives under `data_dir` (default `~/.krabby`) and is plain files —
other tools (doc generators, linters, indexers) are free to read it:

```
~/.krabby/
├── repos/<host>/<group>/.../   # plain git clones
│   └── graphify-out/
│       ├── graph.json          # raw graph (GraphRAG-ready)
│       ├── GRAPH_REPORT.md     # human-readable audit report
│       ├── graph.html          # interactive visualization
│       └── manifest.json       # incremental-update manifest
├── merged/graph.json           # cross-repo merged graph
├── keys/                       # materialized credential SSH keys (0600)
├── docs-vectors/               # embedded documentation vector index
├── sources/<name>/*.md         # synced Custom web / Confluence pages
├── code-vectors/               # embedded source-code vector index
└── state/                      # registry + credentials database
```

`GET /api/v1/repos` returns each repo's local `path` for discovery, and the
artifact endpoints above serve the same files over HTTP for tools that have no
filesystem access.

### Read locks

A background refresh may `git pull` while an external tool reads the clone.
To avoid racing, take a read lock first — refreshes are deferred while it is
held and fire automatically on release or TTL expiry (default 10m, max 1h):

```sh
TOKEN=$(curl -s -X POST localhost:8080/api/v1/repos/git.example.com/myorg/app/-/lock \
  -H 'Content-Type: application/json' -d '{"owner":"docgen","ttl":"5m"}' | jq -r .token)

# ... walk ~/.krabby/repos/myorg/app safely ...

curl -X DELETE localhost:8080/api/v1/repos/git.example.com/myorg/app/-/lock -H "X-Lock-Token: $TOKEN"
```

Locks never block queries or artifact downloads — only git mutations. A crashed
consumer cannot wedge the pipeline: the TTL reaps the lock.

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

## Refresh pipeline

```
webhook / poll / refresh_repo
  → git fetch (new commits?) → git pull
  → graphify update <repo>          # incremental, AST-only, no LLM
  → graphify merge-graphs → merged/graph.json
  → code RAG index (when enabled)
  → generated docs + docs RAG index (when enabled)
```

Repos are also polled at the runtime interval configured in Settings (default
1h); changes apply without restarting krabby.

## Configuration

See [krabby.example.yaml](krabby.example.yaml). Loaded via
[chu](https://github.com/rakunlabs/chu): defaults → `krabby.yaml` (or
`CONFIG_FILE`) → `KRABBY_*` env vars.

Docs RAG and code RAG are independently switchable in the Settings UI. Code RAG
can use its own embedder; when unset it reuses the docs embedder.
The embedded backend keeps docs and code in separate stores so different vector
dimensions are safe.
Generated markdown is stored outside clones under
`data_dir/docs/<owner>/<repo>/`; older in-clone `krabby-docs/` trees are moved
there at startup without regenerating documentation.

## Docker

```sh
docker build -t krabby .
docker run -p 8080:8080 -v krabby-data:/data \
  -v ~/.ssh/id_ed25519:/ssh/key:ro -e KRABBY_GIT_SSH_KEY_PATH=/ssh/key \
  krabby
```

## License

See [LICENSE](LICENSE).
