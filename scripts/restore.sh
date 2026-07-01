#!/usr/bin/env bash
# Restore a pg_dump produced by scripts/backup.sh. DESTRUCTIVE — replaces the
# current database contents. Run from the repo root.
#
#   ./scripts/restore.sh backups/brainiac-<stamp>.sql.gz
set -euo pipefail

# Usage: restore.sh [--force] <dump.sql.gz>
FORCE=0
if [ "${1:-}" = "--force" ]; then FORCE=1; shift; fi
SRC="${1:?usage: restore.sh [--force] <dump.sql.gz>}"
PG_USER="${POSTGRES_USER:-brainiac}"
PG_DB="${POSTGRES_DB:-brainiac}"

[ -f .env ] && set -a && . ./.env && set +a

if [ "$FORCE" != "1" ]; then
	echo "restore: this will OVERWRITE database '$PG_DB' from $SRC"
	read -r -p "type 'yes' to continue: " confirm
	[ "$confirm" = "yes" ] || { echo "aborted"; exit 1; }
fi

gunzip -c "$SRC" | docker compose exec -T db psql -U "$PG_USER" -d "$PG_DB"
echo "restore: done"
