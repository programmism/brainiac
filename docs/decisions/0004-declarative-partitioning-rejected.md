# ADR 0004 — No declarative table partitioning for nodes/edges; keep partial indexes + compaction

**Status:** Accepted (2026-07-19) · Resolves #418 · Supersedes the partitioning idea in #204/#230/#385 ·
Related: SYSTEM.md §10 (2026-07-18 #230, #385)

## Context

Supersession never deletes: an updated node/edge flips `status` `current`→`historical` and the row stays.
Over time the historical tail grows, so #204/#418 proposed **declarative table partitioning** of `nodes`
and `edges` by `status` (a small `current` partition + a `historical`/`proposed` default) to keep the hot
working set physically small.

We already ship the cheap wins: **current-tier partial indexes** (#230) keep the *hot index* the size of
live memory regardless of history depth, and **`kb compact`** (#385, `VACUUM ANALYZE`) reclaims the
dead-tuple heap bloat superseded rows leave behind. #418 asked for the heavier heap separation on top.

## Decision

**Do not** implement declarative partitioning of `nodes`/`edges`. Keep the partial indexes (#230) +
compaction (#385) as the scaling mechanism for the historical tail.

## Why — partitioning breaks a core invariant on each table

Declarative partitioning by `status` (or any column) forces the partition key into every table-level
unique constraint. That collides with this schema in two independent, unavoidable ways (both Postgres
limitations, not implementation effort):

1. **Nodes → lost referential integrity.** A partitioned `nodes` must have its partition key in the
   primary key, so `nodes.id` is no longer *table-level* unique (only per-partition). Then the
   `edges.from_id/to_id → nodes.id` foreign keys **cannot be enforced by the database** — we'd have to drop
   them and rely on the app alone. That trades away a real integrity guarantee.

2. **Edges → broken atomic upsert.** "One *current* edge per `(from_id, to_id, type)`" is enforced by a
   **partial** unique index `WHERE status='current'` (0003), and `InsertEdge` relies on it for its atomic
   `ON CONFLICT (from_id, to_id, type) WHERE status='current' DO UPDATE` — the idempotency behind `link` /
   `remember`. On a partitioned table a *parent-level* unique index must include the partition column, and
   a partial "current-only" index does not — so it can only live on the `current` **partition**, and
   `ON CONFLICT` arbiter inference on the parent `INSERT` cannot use a partition-local index. The atomic
   edge upsert stops working. (Reworking it into a non-atomic SELECT-then-write would regress concurrency.)

So "partition by status" and "keep integrity + the atomic edge upsert" are mutually exclusive here. Neither
`nodes` nor `edges` can be partitioned without giving up a feature that must keep working.

## What we do instead / at extreme scale

- **Now:** #230 (partial current indexes) + #385 (`kb compact`). All features work optimally; the hot path
  is index-pruned to live memory; heap bloat is reclaimable on demand. This is sufficient well beyond
  single-team scale.
- **If the historical tail ever becomes genuinely huge** (≫10M superseded rows and heap size, not index
  size, becomes the bottleneck): the sound approach is **not** declarative partitioning but
  **application-level archival** — move historical rows to separate `*_archive` tables and `UNION` them
  back only in the read paths that need history (as-of reconstruction #200, retention sweep #363), leaving
  the hot tables and their constraints/FKs/upsert untouched. That is a large, deliberate project; open a
  fresh issue for it if the need is ever demonstrated.

## Consequences

- `kb partition` is **not** added. #418 is closed as superseded by #230 + #385.
- Operators manage the historical tail with `kb sweep-retention` (#363) + `kb compact` (#385).
- The trade-off is documented so this isn't re-litigated: partitioning looks attractive until you hit the
  FK/partial-unique walls above.
