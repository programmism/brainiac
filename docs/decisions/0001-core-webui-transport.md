# ADR 0001 — Core↔WebUI transport: REST (net/http + chi), MCP stays separate

**Status:** Accepted (2026-07-01) · Resolves #33 · Related: SYSTEM.md §3, §6.2

## Context
Clients call the core over some transport. Two candidates were on the table (PRD §21):
1. A dedicated **REST API** for the WebUI/CLI/automation, with the **MCP server** as a separate process.
2. **MCP-over-HTTP** (the Go SDK's Streamable HTTP transport) serving the WebUI too, i.e. one HTTP surface.

Facts:
- The MCP server for Claude runs over **stdio** (a subprocess Claude launches). That is the primary,
  best-supported MCP transport and needs no ports/auth of its own.
- The WebUI (v1 read-only) is a static page that needs a plain **JSON HTTP** API it can `fetch()`.
- The CLI already calls the core directly in-process; automation/cron wants simple HTTP too.

## Decision
Use a **dedicated REST API built on `net/http` (Go 1.22 routing) + `chi` middleware**, served by the
**same app binary** as the health endpoints (`internal/server`, `cmd/http`). The **MCP server stays a
separate stdio process** (`cmd/mcp`). Both are thin adapters over the same `internal/core`.

## Why
- **Simplicity for the WebUI/curl:** plain JSON over REST is trivial to consume; MCP framing (JSON-RPC,
  sessions, SSE) is overkill for a read-only dashboard.
- **Right tool per client:** Claude speaks MCP/stdio; browsers speak HTTP. Forcing the WebUI through MCP
  couples an internal protocol to the UI for no benefit.
- **Thin clients, one core:** REST handlers and MCP tools both forward to core — no logic duplication
  (the rule from SYSTEM.md §2/§3). Behaviour cannot drift.
- **Ops:** one HTTP surface (app) to put behind the reverse proxy + auth (M4/#27); MCP/stdio has no
  network exposure to secure.

## Consequences
- REST endpoints live in `internal/server` next to `/healthz`/`/readyz`; `cmd/http` is the deployed HTTP
  binary. Implemented in #20.
- If a browser-based MCP client is ever needed, the SDK's Streamable HTTP transport can be added later
  without touching core — the seam is already at the client layer.
- Reverse proxy + auth (#27) fronts the app's HTTP; MCP is reached via stdio only.
