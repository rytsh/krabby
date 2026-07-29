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

Krabby exposes a streamable HTTP MCP endpoint at `http://localhost:8080/mcp`.

Paste this into your coding agent:

> Add Krabby to my MCP client configuration as a remote streamable HTTP server.
> Name it "krabby", use http://localhost:8080/mcp as the URL, and send the
> X-Krabby-Tool-Profile: full header on every request. Preserve my existing MCP
> servers and tell me if I need to restart the client.

The `full` profile lets your agent configure Krabby and manage credentials in
addition to adding repositories, searching code, reading files, and querying
repository relationships.

For client-specific configuration examples, private repository credentials,
REST endpoints, MCP tools, memory tuning, and development instructions, see
[DETAILS.md](DETAILS.md).

</details>
