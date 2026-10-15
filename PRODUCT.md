# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

- **Developers / platform engineers** who want to self-host krabby (Docker) and wire their coding agent to it. Job: get it running and connected in minutes.
- **AI-agent power users** (OpenCode, Claude Code and similar) who want their agent to have reliable, cited context about their code and docs. Job: add the MCP config and ask better questions.

## Product Purpose

Krabby clones and indexes repositories and other sources, builds a knowledge graph per repo with bag, and serves code search, docs retrieval and relationship analysis to LLM agents over MCP. Success: an agent answers a code question with real paths and lines instead of guesses or whole-repo pastes.

## Positioning

- **Single Go binary, self-hosted.** One process; data lives as plain files under `~/.krabby` (clones, `graph.json`, reports, vectors). No LLM key needed for code extraction (AST-based).
- **Graph-backed truth.** Definitions and references come from real graph edges with path and line, answered in-process by a native Go graph engine; regex hits carry exact line/column.
- **Everything in one index.** Repos plus web pages, docs sites, Confluence, Jira, RSS, JSON APIs, S3, WebDAV, Git provider issues/PRs, Slack/Mattermost/Teams, SQL, Notion, Google Drive, SharePoint and more behind one MCP surface.

## Operating Context

Runs as a container (`ghcr.io/rytsh/krabby`) or binary; Web UI on :8080; MCP at `/mcp` (read-only), `/mcp/api` (API discovery and calls), `/mcp/admin` (mutations). REST API and provider-neutral `/webhook/git`. Background scheduler keeps indexes fresh.

## Capabilities and Constraints

See README.md and DETAILS.md for the authoritative feature list (MCP tools, REST API). Optional: LLM-generated docs, semantic search with an embedder. Shared agent memory, Big Picture workspaces, API catalog (OpenAPI/gRPC).

## Brand Commitments

- The halftone comic "krabby" wordmark (`_docs/public/krabby.webp`) stays.

## Evidence on Hand

- Wordmark: `_docs/public/krabby.webp`; favicon `_docs/public/favicon.svg`.
- No testimonials, customer logos, benchmarks or star counts are on hand; do not fabricate them.
- License: MIT. Author: Eray Ates. Repo: https://github.com/rytsh/krabby.

## Product Principles

1. Evidence over guesses: every answer traces to a path and a line.
2. Read-only by default; mutation is opt-in and separated.
3. Your infra, your files: nothing is hidden in a proprietary store.
4. Cheap to run: no model required for the core path.
