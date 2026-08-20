<img src="./_docs/public/krabby.webp" width="360" alt="Krabby" />

[![License](https://img.shields.io/github/license/rytsh/krabby?color=red&style=flat-square)](https://raw.githubusercontent.com/rytsh/krabby/main/LICENSE)
[![Coverage](https://img.shields.io/sonar/coverage/rytsh_krabby?logo=sonarcloud&server=https%3A%2F%2Fsonarcloud.io&style=flat-square)](https://sonarcloud.io/summary/overall?id=rytsh_krabby)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/rytsh/krabby/test.yml?branch=main&logo=github&style=flat-square&label=ci)](https://github.com/rytsh/krabby/actions)
[![Container](https://img.shields.io/badge/ghcr.io-krabby-blue?logo=docker&style=flat-square)](https://github.com/rytsh/krabby/pkgs/container/krabby)
[![Web](https://img.shields.io/badge/web-document-blueviolet?style=flat-square)](https://rytsh.github.io/krabby/)

Krabby indexes your repositories and lets AI agents search code, read documentation,
and understand relationships through MCP.

## Features

- Repository indexing with automatic background refreshes
- Fast code search and file access
- Architecture and relationship analysis with knowledge graphs
- Optional generated documentation and semantic search
- Web, Confluence, and Jira source indexing
- API catalog: OpenAPI/Swagger and gRPC endpoints, browsable and searchable, with per-service overrides
- Web UI, REST API, and MCP support

## Run with Docker

With Docker Compose:

```sh
docker compose --project-name krabby up -d --pull always
```

To stop and remove Krabby while keeping its data:

```sh
docker compose --project-name krabby down

# Also permanently delete all Krabby data:
# docker compose --project-name krabby down --volumes
```

Or with Docker directly:

```sh
mkdir -p krabby-data

docker run -d \
  --name krabby \
  -p 8080:8080 \
  -v "$(pwd)/krabby-data:/data" \
  ghcr.io/rytsh/krabby:latest
```

Open [http://localhost:8080](http://localhost:8080), then add repositories from
the UI. The `krabby-data` volume (or directory when using `docker run`) keeps
repositories, indexes, and settings between container restarts.

> For base path use **KRABBY_SERVER_BASE_PATH** environment variable, e.g. `-e KRABBY_SERVER_BASE_PATH=/krabby` to run behind a reverse proxy.

## Add MCP

Go to **About** section in the UI and follow the instructions to add Krabby to your MCP client configuration.

<details>
<summary>Click for MCP configuration</summary>

Krabby exposes three independent streamable HTTP MCP catalogs:

- `http://localhost:8080/mcp` — read-only code, graph, files, history, and docs
- `http://localhost:8080/mcp/api` — API catalog discovery and live endpoint calls
- `http://localhost:8080/mcp/admin` — repository, credential, source, API, queue, and settings administration

Paste this into your coding agent:

> Add Krabby to my MCP client configuration as three remote streamable HTTP
> servers named "krabby", "krabby-api", and "krabby-admin", using
> http://localhost:8080/mcp, http://localhost:8080/mcp/api, and
> http://localhost:8080/mcp/admin. Preserve my existing MCP servers and tell me
> how to enable only the catalogs needed for a task.

The catalogs do not overlap. Keep `krabby` enabled for normal codebase work,
enable `krabby-api` only when an agent should discover or call catalogued APIs,
and enable `krabby-admin` only while it should be allowed to add, change, or
delete Krabby-managed resources.

For client-specific configuration examples, private repository credentials,
REST endpoints, MCP tools, memory tuning, and development instructions, see
[DETAILS.md](DETAILS.md).

</details>
