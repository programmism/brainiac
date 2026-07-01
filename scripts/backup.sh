#!/usr/bin/env bash
# Daily backup: one consistent pg_dump covers graph + vectors + provenance
# (single DB, no cross-store sync — SYSTEM.md §16). Run from the repo root.
#
#   ./scripts/backup.sh [dest_dir]
#
# Schedule with cron, e.g. daily at 02:00:
#   0 2 * * * cd /path/to/brainiac && ./scripts/backup.sh >> backups/backup.log 2>&1
set -euo pipefail

DEST="${1:-backups}"
RETENTION="${BACKUP_RETENTION:-14}"
PG_USER="${POSTGRES_USER:-brainiac}"
PG_DB="${POSTGRES_DB:-brainiac}"

# Load .env if present (for POSTGRES_* defaults).
[ -f .env ] && set -a && . ./.env && set +a

mkdir -p "$DEST"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="$DEST/brainiac-$STAMP.sql.gz"

echo "backup: dumping $PG_DB → $OUT"
docker compose exec -T db pg_dump -U "$PG_USER" -d "$PG_DB" --clean --if-exists | gzip > "$OUT"
echo "backup: wrote $(du -h "$OUT" | cut -f1)"

# Retention: keep the newest $RETENTION dumps.
count=$(ls -1 "$DEST"/brainiac-*.sql.gz 2>/dev/null | wc -l)
if [ "$count" -gt "$RETENTION" ]; then
	ls -1t "$DEST"/brainiac-*.sql.gz | tail -n +"$((RETENTION + 1))" | while read -r old; do
		echo "backup: pruning $old"
		rm -f "$old"
	done
fi
