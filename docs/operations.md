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
```cron
0 2 * * * cd /path/to/brainiac && ./scripts/backup.sh >> backups/backup.log 2>&1
```

## Restore
**Destructive** — replaces the current database contents.
```bash
./scripts/restore.sh backups/brainiac-<stamp>.sql.gz
```
The script prompts for confirmation, then pipes the dump into `psql` in the `db` container. After a
restore the app picks up the data immediately (schema + vectors + graph all came from the one dump).

## Notes
- Store backups off-box (e.g. sync `backups/` to object storage) for real disaster recovery.
- `pgvector` data (halfvec embeddings) is included in the dump — no separate vector export needed.
- For point-in-time recovery (PITR) at larger scale, enable WAL archiving on Postgres; the daily dump is
  the simple default for the prototype/team tier.
