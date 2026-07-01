#!/usr/bin/env bash
# Restore a pg_dump produced by scripts/backup.sh. DESTRUCTIVE — replaces the
# current database contents. Run from the repo root.
#
#   ./scripts/restore.sh backups/brainiac-<stamp>.sql.gz
set -euo pipefail

SRC="${1:?usage: restore.sh <dump.sql.gz>}"
PG_USER="${POSTGRES_USER:-brainiac}"
PG_DB="${POSTGRES_DB:-brainiac}"

[ -f .env ] && set -a && . ./.env && set +a

echo "restore: this will OVERWRITE database '$PG_DB' from $SRC"
read -r -p "type 'yes' to continue: " confirm
[ "$confirm" = "yes" ] || { echo "aborted"; exit 1; }

gunzip -c "$SRC" | docker compose exec -T db psql -U "$PG_USER" -d "$PG_DB"
echo "restore: done"
