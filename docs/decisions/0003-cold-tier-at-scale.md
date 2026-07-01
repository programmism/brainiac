# ADR 0003 — Cold-tier vector storage at scale

**Status:** Accepted (2026-07-01) · Resolves #34 · Related: SYSTEM.md §9, §7, PRD §13, §15, §21

## Context
The binding constraint is the vector index in RAM, not disk (SYSTEM.md §9). We need a plan for when the
corpus outgrows a single pgvector node — and the two-store join story if chunks ever leave Postgres.
The signal is **observed**, not a row count: index approaching ~½ RAM, p95 latency climbing (spill to
disk), or golden-set recall degrading.

## Decision
**Stay on single-node Postgres + pgvector as long as the observed signals allow, escalating in this
order.** Only adopt a dedicated vector DB for the *cold tier* when quantization + tiering are exhausted.

1. **Selection** (already the default) — the strongest lever; keeps low-signal chunks out entirely.
2. **Quantization** — `halfvec` (today) → `int8` (×4) → `binary` (×32) with a re-rank step. Schema is
   already `halfvec`; moving further is a data job (re-embed/re-encode from stored raw text), not a
   schema reshape.
3. **Matryoshka dimensionality reduction** — nomic supports truncating 768→256 for ~×3 saving at a small
   recall cost; validate on the golden set (#29) before adopting.
4. **Hot/cold tiering** (already modeled) — the HNSW index covers only `tier='hot'`; cold chunks are
   retained (raw text + score) but excluded from default search. This bounds the *in-RAM* index while
   keeping the archive queryable on demand.
5. **Dedicated cold-tier vector DB** — only at ~100M+ vectors (≈1 TB text). Move the **cold** tier to
   Qdrant/Milvus; keep the **hot** tier and the whole **graph** in Postgres.

## The two-store join, if the cold tier leaves Postgres
Today `recall` joins graph→chunks by `source_id` in one SQL query. If cold chunks move to Qdrant/Milvus,
that join becomes **application-level for the cold tier only**:
1. Vector search the external store → returns chunk ids + scores.
2. Fetch those chunks' provenance/graph links from Postgres by id (indexed lookup).
Hot-tier recall stays a single in-DB join. The core's `recall` already returns an evidence bundle
assembled in code, so this is an internal change to the store layer, not an API change.

## Why not a dedicated vector DB now
- One DB = one transaction, one backup, one consistent snapshot (SYSTEM.md §3.2). We do not pay the
  cross-store consistency + ops cost until the corpus actually demands it.
- pgvector + quantization + tiering comfortably covers the ~10M-vector single-node tier, which is far
  beyond the reference deployment.

## Consequences
- No action now. The escalation ladder is the plan; watch the §9 signals and the golden set (#29).
- When the cold tier is externalized, add an `Embedder`-agnostic cold-store adapter behind the existing
  store layer; `recall` composes hot (pg) + cold (external) results. Interface, not core, changes.
