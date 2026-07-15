# Production-readiness audit — RESOLVED (M5 complete)

> **Status:** this was the M5 hardening audit of the v1.3.0 build. **Every gap below (#68–#87) has since
> shipped** and M5 is complete (current release ≥ v1.35.0). It is kept as a hardening history, not a live
> blocker list. For the *current* forward-looking assessment and roadmap, see the product-evaluation epics
> (#202–#209) and the roadmap tracking issue (#283).

A code-grounded audit (as of v1.3.0) across five dimensions: security & auth, data integrity, reliability,
retrieval quality, and observability/ops.

**Historical bottom line (v1.3.0):** the M0–M4 roadmap was functionally complete and CI-green but not yet
safe to point at real data — blockers were concentrated in data actualization, security defaults, retrieval
relevance, and ingest resilience. **All were resolved in M5; the tables below are the record of that work.**

## P0 — blocked production *(all resolved in M5)*

| # | Gap | Why it matters |
|---|---|---|
| #68 | **Re-import never deletes stale chunks** (insert-only; no delete-by-`source_uri`; `source_modified_at` never populated) | Every edit permanently accumulates stale, contradictory hot-tier chunks that keep ranking in search. Search quality degrades monotonically with normal use. |
| #69 | **No app-layer auth; app port open by default** | Write endpoints (`/api/merge`, `confirm`, `flag-stale`) mutate the graph unauthenticated; default `docker compose up` exposes `0.0.0.0:8080`. Auth is only in the *optional* Caddy profile. |
| #70 | **No relevance floor in retrieval** (`ORDER BY <=> LIMIT k`, no min-similarity) | Off-topic queries return confidently-cited but irrelevant evidence — the biggest source of wrong answers. |
| #71 | **Edge insertion has no uniqueness** | `link` twice → duplicate current edges; inflates rollups and silently doubles the graph on every re-run/extractor pass. |
| #72 | **Ingest not resilient** (no Ollama retry; not transactional per doc) | One transient Ollama/Notion blip aborts the whole `kb import`; one-by-one commits leave partial state with no checkpoint. |

## P1 — fix before real use

| # | Gap |
|---|---|
| #73 | Recall graph traversal unbounded (`EdgesForNode` no LIMIT) — a hub node floods the evidence bundle + hundreds of DB round-trips |
| #74 | No embedding-dimension validation — a non-768-dim model fails at insert with an opaque pgvector error |
| #75 | Observability: no request logging; the two ★ scaling metrics (index-size-vs-RAM, p95 latency) are declared but **not measured**; no `/version` over HTTP |
| #76 | `content_hash` dedup is global, not per-source — identical text across sources loses provenance |
| #77 | Query endpoints 500 (and leak `err.Error()`) when Ollama is down instead of 503 |
| #78 | Startup does not retry DB connect/migrate — crash-loops if Postgres isn't up yet |
| #79 | Notion connector doesn't honor 429/`Retry-After` |
| #80 | Compose: no resource limits (OOM on the 4 GB box), no log rotation, app port still exposed under the proxy |

## P2 — hardening

| # | Gap |
|---|---|
| #81 | Chunking hard-splits mid-word/rune, no overlap |
| #82 | Empty query not validated on the MCP path (embeds `""`) |
| #83 | Density selector: first-word entities missed; English-only stopwords |
| #84 | Evidence join surfaces cold-tier chunks that search excludes |
| #85 | Config validation shallow (BaseURL/Provider/Model); DSN may echo in errors |
| #86 | Backups: manual scheduling, interactive-only restore, no off-box/verify |
| #87 | HTTP hardening: unbounded request body, missing Write/Idle timeouts, no `/metrics` |

## Method
Five independent reviewers each audited one dimension against the actual code (file:line grounded), then
the findings were de-duplicated and prioritized. This document is the synthesis; the issues carry the
detail and the fix sketch.

## Order of work *(completed)*
P0 first, in this order: **#68 actualization → #72 ingest resilience** (they shared the per-doc
transaction) → **#71 edge uniqueness → #70 relevance floor → #69 secure-by-default**, then P1, then P2 —
all shipped. The *next* generation of hardening (retrieval quality, scale indexing, connector breadth,
security identity/audit, observability) is tracked in the product-evaluation epics #202–#209.
