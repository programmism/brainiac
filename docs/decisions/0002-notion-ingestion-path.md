# ADR 0002 — Notion ingestion: native API connector (not export)

**Status:** Accepted (2026-07-01) · Resolves #32 · Related: SYSTEM.md §7, PRD §7.1, §21

## Context
The reference domain ingests from Notion. Two ways to get documents in (PRD §21 flags this as open):
1. **Native Notion API** — an internal integration (token) reads pages/blocks over HTTPS.
2. **Export-based** — a manual Markdown/HTML export dump is parsed offline.

The connector seam (`plugins.SourceConnector`) requires two things: `Fetch()` (pull documents) and
`Watch()` (detect changes, to power refresh/actualization — SYSTEM.md §7.1).

## Decision
Implement a **native Notion API connector** for v1 (#19). Keep export parsing only as a possible bulk
**backfill** path, not the primary route.

## Why
- **Actualization needs change detection.** The Notion API exposes `last_edited_time` on every page;
  `Watch()` can poll the `search`/`databases.query` endpoints filtered/sorted by `last_edited_time` and
  emit `Change{Upserted}` for pages edited since the last sync. A static export has no change signal —
  it would force full re-ingest every time, breaking the refresh story.
- **Structured provenance.** The API returns stable `page_id`/`block_id` and page URLs, which map
  cleanly onto `source_uri` (page URL) and `source_locator` (`{page_id, block_id, heading}`) for
  citation. Export dumps lose these ids.
- **No manual step.** "Very easy to deploy/operate" (SYSTEM.md §4) argues against a human running exports.

## Design notes for #19 (implementation)
- **Auth:** an internal integration token from config (`sources[].token` / env). The integration must be
  shared with the target pages/databases. *Verify current token scopes and connector availability at
  build time (PRD §21).*
- **Fetch:** enumerate pages via `POST /v1/search` (or `databases.query` for a specific DB), paginate
  with `start_cursor`/`has_more`, then read each page's block tree via `GET /v1/blocks/{id}/children`
  (recursing into child blocks), flattening rich-text to plain text → `RawDoc.Text`.
- **Watch:** same enumeration, filtered/sorted by `last_edited_time`; emit changes newer than the last
  cursor timestamp.
- **API version:** send the `Notion-Version` header (pin a known-good date). Handle `429` with backoff.
- **Selection:** chunking + the density-filter selector (#17) run *after* Fetch, in the ingest pipeline
  (#18) — the connector only yields raw docs. Per-chunk selection keeps value hidden inside watery pages.
- **Testing:** unit-test against a mocked Notion API (httptest) — search pagination, block flattening,
  and `last_edited_time` change detection — with no live Notion needed.

## Consequences
- The connector depends only on an HTTP client + token; no SDK lock-in.
- Export backfill, if ever needed for the archive tier, is a separate connector variant reusing the same
  chunk/select/embed pipeline.
