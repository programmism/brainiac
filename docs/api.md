# HTTP API reference

The app serves a JSON REST API on `http.addr` (default `:8080`). All `/api/*`
errors are JSON: `{"error": "..."}` with the appropriate status — never plain
text. Reads are open in Layer 1 (protect via the reverse proxy); writes require a
bearer token. Under Layer 2 (`principals:`) **every** `/api` call needs a
principal token and the operator-only write group is not mounted (see
[security.md](security.md)).

Primary agent access is over **MCP** (stdio), not this API — see
[agent-memory.md](agent-memory.md). This surface backs the WebUI and integrations.

## Health & meta (no auth)

| Method · path | Returns |
|---|---|
| `GET /healthz` | `{status, version}` — liveness |
| `GET /readyz` | `{db, embedder}` — readiness; `db:"ok"` gates 200, embedder is `ok`/`model-missing`/`unreachable`/`not-configured` (reported, not fatal) |
| `GET /metrics` | Prometheus text: request latency histogram, vector-index bytes, node/edge/chunk gauges, container memory |
| `GET /api/capabilities` | `{writable, auth_required}` — public even under isolation, so the UI can prompt for a token |

## Reads (`GET /api/...`)

| Path | Query | Returns |
|---|---|---|
| `/health` | — | corpus/graph health metrics + version + p50/p95 latency |
| `/system` | — | process + container memory, DB size, pool saturation, derived status |
| `/search` | `q` (required), `k`, `project` | ranked chunk hits (hybrid dense+FTS) |
| `/recall` | `q` (required), `project` | evidence bundle: chunks + nodes + edges (with `why`) + evidence + `scope`/`scope_fallback` |
| `/node` | `name` or `id` (one required), `project`, `as_of` | one entity's full record + edges; `as_of` (RFC3339 / `YYYY-MM-DD`) returns its state at a past date |
| `/graph` | `limit` | bounded node/edge snapshot for visualization |
| `/consolidate` | — | librarian report (merge/split/conflict/stale/rollup candidates); read-only |
| `/proposals` | `limit` | pending local-extractor proposals (empty unless enabled) |
| `/logs` | `limit` | recent app + access logs (only when a log sink is set; **not** mounted under isolation) |

## Writes (`POST /api/...`, bearer auth; Layer 1 only)

Require `Authorization: Bearer <AUTH_TOKEN>` and `clients.webui=interactive`.
Error mapping: `403` forbidden namespace, `429` quota exceeded, `503` embedder
down, `400` bad request, `500` otherwise.

| Path | Body | Effect |
|---|---|---|
| `/merge` | `{keep, drop}` | merge drop node into keep |
| `/split` | `{node_id, axis, routes}` | split a conflated node along an axis |
| `/edges/{id}/confirm` | — | clear stale flag, refresh confirmation |
| `/edges/{id}/flag-stale` | — | flag an edge for review |
| `/edges/{id}/retire` | — | mark an edge historical |
| `/proposals/{nodes\|edges}/{id}/approve` | — | promote a proposal to current |
| `/proposals/{nodes\|edges}/{id}/reject` | — | retire a proposal |

## Auth

- **Layer 1:** reads open; writes need `Authorization: Bearer <AUTH_TOKEN>`.
- **Layer 2:** all `/api` (except `/capabilities`) need `Authorization: Bearer
  <principal token>`; reads are walled to the principal's namespaces. Curation
  writes are operator-only and not mounted; namespace writes flow through MCP.
