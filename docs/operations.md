# Operations: backup & restore

Brainiac stores everything — the graph, vectors, and provenance — in **one Postgres**, so a single
`pg_dump` is a complete, consistent backup (SYSTEM.md §16). No cross-store sync to coordinate.

## Backup
```bash
./scripts/backup.sh            # → backups/brainiac-<UTC-stamp>.sql.gz
```
- Uses `pg_dump --clean --if-exists` through the `db` container; output is gzipped.
- Retention: keeps the newest `BACKUP_RETENTION` dumps (default 14).

### Schedule (daily)
Either a host cron:
```cron
0 2 * * * cd /path/to/brainiac && ./scripts/backup.sh >> backups/backup.log 2>&1
```
…or the bundled backup sidecar (dumps daily to `./backups`, keeps the newest 14):
```bash
docker compose --profile backup up -d backup
```

## Restore
**Destructive** — replaces the current database contents.
```bash
./scripts/restore.sh backups/brainiac-<stamp>.sql.gz          # prompts for confirmation
./scripts/restore.sh --force backups/brainiac-<stamp>.sql.gz  # unattended (DR scripts)
```
The script pipes the dump into `psql` in the `db` container. After a
restore the app picks up the data immediately (schema + vectors + graph all came from the one dump).

## Notes
- Store backups off-box (e.g. sync `backups/` to object storage) for real disaster recovery.
- `pgvector` data (halfvec embeddings) is included in the dump — no separate vector export needed.
- For point-in-time recovery (PITR) at larger scale, enable WAL archiving on Postgres; the daily dump is
  the simple default for the prototype/team tier.

## Hot / cold tiering (#231)

The hot tier is HNSW-indexed and searched; cold is an archive (no vector index, not
in the default search path). Content is promoted to hot on ingest by the density
selector. On a large KB, cap the hot tier so its index stays within RAM (the §9 ★
ratio) with an **opt-in age-based demotion**: set `tiering.max_hot_age`
(env `TIERING_MAX_HOT_AGE`, a Go duration like `4320h` = 180d) and run periodically:

```bash
kb sweep-tiers      # archives hot chunks older than max_hot_age to cold
```

Demoted chunks keep their text + embedding — cold is **archival and re-promotable**
(`SetChunkTier` / a reindex), not deleted. It's a no-op until `max_hot_age` is set.
An on-demand cold-search path (query the archive when hot misses) is tracked in #365.

## Right-to-erasure (#272)

Supersede/merge keep history; for a GDPR erasure you need a real delete. Hard-delete
at fact granularity with:

```bash
kb erase --node <id>        # an entity and all its edges
kb erase --source <uri>     # every chunk + edge from a document
# add --force to skip the confirmation prompt (unattended)
```

This is irreversible (no history row is kept) and audited (`erase_node` / `erase_source`
in the audit log). `--node` respects the Layer-2 wall (a principal can only erase
its own namespace); `--source` is an operator action. It is intentionally not an
MCP tool, so an agent can't erase mid-conversation. Retention-policy auto-purge and
at-rest encryption are tracked in #363.

## Updating safely (#261)

Migrations are **forward-only** (no down scripts) and run **automatically when the app boots**. That's what
keeps deploys one-command — but it means the moment a new image starts, the schema is migrated and there is
no in-app way back. So always **snapshot before you migrate**:

```bash
make update            # or: ./scripts/update.sh
```

`update.sh` (1) takes a pre-migrate `pg_dump` into `backups/pre-update/`, (2) `docker compose pull` + `up -d`
(the new binary runs pending migrations on boot), then (3) waits for `/brainiac healthcheck`. If the app
doesn't come up healthy it prints a rollback recipe (it does **not** auto-roll-back — a half-applied change
may need a look first): re-pin the previous `BRAINIAC_VERSION`, `restore.sh --force` the pre-migrate dump,
`up -d`.

### Expand / contract migration discipline

Because an old binary may briefly run against the new schema (rolling restart) and a rollback restores the
*old* binary against the *migrated* schema, every migration must be **backward-compatible with the previous
release**. Split any breaking change across two releases:

- **Expand (release N):** only *additive*, nullable/defaulted changes — add a column/table/index, backfill,
  start writing both old and new shapes. The prior binary keeps working because nothing it reads was removed
  or made stricter.
- **Contract (release N+1):** once every running binary is on N, remove the old column, add the `NOT NULL`,
  drop the compatibility shim. Never drop or rename a column, nor add a `NOT NULL` without a default, in the
  same release that starts using it.

This is what makes the snapshot a genuine rollback point rather than a one-way door.

## Tuning Postgres at scale (#232)
The defaults are sized for the prototype/team tier. As the corpus and write rate
grow, three knobs matter. (For pointing at a **managed** Postgres and connection
pooling, see [managed-postgres.md](managed-postgres.md).)

**Autovacuum for churn.** Brainiac's write pattern creates dead tuples faster than a
typical app: supersede/merge **flip rows** `current → historical` (not deletes, but
updates), and the ingest reconcile **deletes** stale chunks. Make autovacuum keep up
on the hot tables — per-table so you don't disturb the whole cluster:

```sql
ALTER TABLE chunks SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_analyze_scale_factor = 0.02);
ALTER TABLE nodes  SET (autovacuum_vacuum_scale_factor = 0.05);
ALTER TABLE edges  SET (autovacuum_vacuum_scale_factor = 0.05);
```

(The default `0.2` scale factor waits until 20% of a table is dead — too lax for
churny tables; `0.05` vacuums at 5%.)

**HNSW index maintenance.** The hot-tier vector index is partial
(`chunks_embedding_hot_idx WHERE tier = 'hot'`, plus `nodes_summary_embedding_idx`).
Heavy re-embedding / tier flips can bloat it and drift recall. After a large
backfill or re-embed, rebuild without taking a write lock:

```sql
REINDEX INDEX CONCURRENTLY chunks_embedding_hot_idx;
REINDEX INDEX CONCURRENTLY nodes_summary_embedding_idx;
```

**HNSW build parameters (#233).** The indexes ship with pgvector's defaults
(`m=16`, `ef_construction=64`). Ahead of a large (10M+) tier, a denser graph
recalls better — raise them via config (`index.hnsw_m` / `index.hnsw_ef_construction`,
or env `HNSW_M` / `HNSW_EF_CONSTRUCTION`; `ef_construction` must be ≥ `2·m`) and
apply with:

```bash
kb reindex   # or: docker compose exec app /kb reindex
```

`reindex` rebuilds both indexes **online** (`CREATE INDEX CONCURRENTLY` → drop →
rename), so search keeps serving throughout. `REINDEX` alone rebuilds with the
*existing* params, so use `kb reindex` when you're *changing* `m`/`ef_construction`.
A higher `m`/`ef_construction` costs build time and index size (watch the ½-RAM
ratio below).

Watch `brainiac_vector_index_bytes` vs container RAM (the ★ ratio, §9 / alert
`BrainiacVectorIndexExceedsHalfRAM`) — when the index outgrows ~½ RAM, query p95
rises as it spills; raise memory or shrink the hot tier.

**Point-in-time recovery (PITR).** The daily `pg_dump` (below) is the simple default
and restores to the last snapshot. For finer recovery on self-hosted Postgres,
enable **WAL archiving** and base backups:

```
# postgresql.conf
wal_level = replica
archive_mode = on
archive_command = 'test ! -f /archive/%f && cp %p /archive/%f'   # ship WAL off-box
```

Take a `pg_basebackup -D /base -Ft -z` periodically; restore by unpacking the base
backup + replaying archived WAL to a `recovery_target_time`. Managed Postgres does
all of this for you (see managed-postgres.md).

## Monitoring & alerts (#264)
The app exposes Prometheus metrics at `/metrics` (per-route latency + error counts,
graph-health gauges, container memory, vector-index size). Ship-ready alerting
rules and a per-alert **runbook** live under `deploy/monitoring/`:

- [`brainiac.rules.yml`](../deploy/monitoring/brainiac.rules.yml) — alerts (down,
  5xx rate, search p95, index-exceeds-½-RAM, memory-near-limit, stale-edges).
- [`prometheus-scrape.yml`](../deploy/monitoring/prometheus-scrape.yml) — scrape +
  `rule_files` wiring.
- **[runbook.md](runbook.md)** — what each alert means and the first response.

⚠️ `/metrics` is unauthenticated and at the server root — scrape it over the
internal network, never expose it publicly.
