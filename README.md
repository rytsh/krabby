<img src="./assets/krabby.webp" width="360" />

krabby multi-repo [graphify](https://github.com/safishamsi/graphify) knowledge graphs
served over MCP. Point it at your repositories; it clones them, builds a code
knowledge graph per repo (plus a merged cross-repo graph), keeps them fresh in
the background, and lets any MCP-capable LLM agent query them.

```
                     ┌───────────────────────────────────────┐
 LLM/Agent ──MCP───► │ krabby (Go)                           │
 (streamable HTTP)   │  ├─ MCP tools (manage + query)        │──► git clone/pull
                     │  ├─ REST API + GitHub webhook         │──► graphify update
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
curl -X POST localhost:8080/api/v1/repos -d '{"url":"git@github.com:rakunlabs/ada.git"}'
curl localhost:8080/api/v1/repos
```

MCP endpoint for agents (opencode, Claude Desktop, etc.): `http://localhost:8080/mcp`
(streamable HTTP; set `mcp.api_key` to require `X-Api-Key` / `Authorization: Bearer`).

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

## MCP tools

| Tool | Purpose |
| --- | --- |
| `list_repos` / `add_repo` / `remove_repo` | Manage tracked repositories |
| `refresh_repo` | Pull + rebuild graph in the background |
| `repo_status` | Build state, last commit, last error |
| `set_credential` / `list_credentials` / `remove_credential` | Per-host / per-org git credentials |
| `lock_repo` / `unlock_repo` | TTL-bounded read locks for external consumers |
| `query_graph` | BFS/DFS search over one repo or the merged graph |
| `get_node` / `get_neighbors` / `get_community` | Node-level inspection |
| `god_nodes` / `graph_stats` / `shortest_path` | Graph-level analysis |
| `search_docs` / `list_docs` / `get_doc` | Search and read generated Markdown documentation |
| `search_code` | Normal bw full-text or semantic source search with locations and pagination |
| `get_docs_config` / `set_docs_config` | Read or live-update docs and code RAG settings |
| `test_llm` / `test_embedder` / `test_code_embedder` | Validate model endpoints without saving |

All query tools take an optional `repo` (`owner/name`); omit it to query the
merged cross-repo graph.

## REST API

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness |
| `GET /api/v1/repos` | List repos |
| `POST /api/v1/repos` `{"url","branch"}` | Track a repo |
| `GET /api/v1/repos/{owner}/{name}` | Repo status |
| `DELETE /api/v1/repos/{owner}/{name}` | Untrack + delete clone |
| `POST /api/v1/repos/{owner}/{name}/refresh` | "This repo changed" trigger |
| `POST /api/v1/repos/{owner}/{name}/lock` `{"owner","ttl"}` | Take a read lock (returns token) |
| `GET /api/v1/repos/{owner}/{name}/lock` | Lock status |
| `DELETE /api/v1/repos/{owner}/{name}/lock` + `X-Lock-Token` | Release the lock |
| `GET /api/v1/repos/{owner}/{name}/graph` | Raw `graph.json` of one repo |
| `GET /api/v1/repos/{owner}/{name}/report` | `GRAPH_REPORT.md` audit report |
| `GET /api/v1/repos/{owner}/{name}/html` | Interactive graph visualization |
| `GET /api/v1/graph` | Merged cross-repo `graph.json` |
| `GET /api/v1/docs/search?q=&repo=&top=` | Semantic documentation search |
| `GET /api/v1/code/search?q=&repo=&top=` | Semantic source-code snippet search |
| `GET/PUT /api/v1/docs/config` | Read/update docs and code RAG settings |
| `GET /api/v1/credentials` | List credential patterns (secrets never returned) |
| `PUT /api/v1/credentials` `{"pattern","secret","kind","username"}` | Store a credential |
| `DELETE /api/v1/credentials?pattern=...` | Remove a credential |
| `POST /webhook/github` | GitHub push webhook (HMAC verified) |

## Data layout & external tools

Everything lives under `data_dir` (default `~/.krabby`) and is plain files —
other tools (doc generators, linters, indexers) are free to read it:

```
~/.krabby/
├── repos/<owner>/<name>/       # plain git clones
│   └── graphify-out/
│       ├── graph.json          # raw graph (GraphRAG-ready)
│       ├── GRAPH_REPORT.md     # human-readable audit report
│       ├── graph.html          # interactive visualization
│       └── manifest.json       # incremental-update manifest
├── merged/graph.json           # cross-repo merged graph
├── keys/                       # materialized credential SSH keys (0600)
├── docs-vectors/               # embedded documentation vector index
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
TOKEN=$(curl -s -X POST localhost:8080/api/v1/repos/myorg/app/lock \
  -H 'Content-Type: application/json' -d '{"owner":"docgen","ttl":"5m"}' | jq -r .token)

# ... walk ~/.krabby/repos/myorg/app safely ...

curl -X DELETE localhost:8080/api/v1/repos/myorg/app/lock -H "X-Lock-Token: $TOKEN"
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

# Token for one GitHub org (https clones):
curl -X PUT localhost:8080/api/v1/credentials \
  -d '{"pattern":"github.com/myorg","secret":"ghp_..."}'
```

Or let the LLM do it over MCP with `set_credential`. SSH keys are materialized
under `data_dir/keys/` with 0600 perms; tokens are fed to git via a credential
helper (never on argv). Secrets are never returned by any API. The global
`git.ssh_key_path` config remains as a fallback for unmatched ssh URLs.

## Refresh pipeline

```
webhook / poll / refresh_repo
  → git fetch (new commits?) → git pull
  → graphify update <repo>          # incremental, AST-only, no LLM
  → graphify merge-graphs → merged/graph.json
  → code RAG index (when enabled)
  → generated docs + docs RAG index (when enabled)
```

Repos are also polled every `git.poll_interval` (default 1h).

## Configuration

See [krabby.example.yaml](krabby.example.yaml). Loaded via
[chu](https://github.com/rakunlabs/chu): defaults → `krabby.yaml` (or
`CONFIG_FILE`) → `KRABBY_*` env vars.

Docs RAG and code RAG are independently switchable in the Settings UI. Code RAG
can use its own `code_embedder`; when that block is unset it reuses `embedder`.
The embedded backend keeps docs and code in separate stores so different vector
dimensions are safe. With Qdrant, code uses `<collection>-code`.

## Docker

```sh
docker build -t krabby .
docker run -p 8080:8080 -v krabby-data:/data \
  -v ~/.ssh/id_ed25519:/ssh/key:ro -e KRABBY_GIT_SSH_KEY_PATH=/ssh/key \
  krabby
```

## License

See [LICENSE](LICENSE).
