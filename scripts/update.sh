#!/usr/bin/env bash
# Safe update: snapshot the database BEFORE the new version migrates it, then
# pull and recreate. Run from the repo root.
#
#   ./scripts/update.sh
#
# Why the snapshot matters (#261): migrations are FORWARD-ONLY (no down scripts)
# and run automatically when the app boots, so once the new image starts there is
# no in-app way back to the old schema. The pre-migrate dump this takes is the
# rollback point. To move versions, set BRAINIAC_VERSION in .env to the new tag
# first (or leave it at "latest"), then run this.
set -euo pipefail

SNAPSHOT_DIR="${SNAPSHOT_DIR:-backups/pre-update}"

# Load .env so BRAINIAC_VERSION / POSTGRES_* match the running stack.
[ -f .env ] && set -a && . ./.env && set +a

# The version currently pinned — the target of a rollback if the update fails.
PREV_VERSION="${BRAINIAC_VERSION:-latest}"

# 1. Snapshot BEFORE migrating. This dump is the only way back to the old schema.
echo "update: snapshotting database before migrate → $SNAPSHOT_DIR"
./scripts/backup.sh "$SNAPSHOT_DIR"

# 2. Pull the new image and recreate; the app runs pending migrations on boot.
echo "update: pulling new images…"
docker compose pull
echo "update: recreating services (migrations run on boot)…"
docker compose up -d

# 3. Wait for the app to become healthy (it self-probes via /brainiac healthcheck).
echo "update: waiting for app health…"
ok=0
for _ in $(seq 1 30); do
	if docker compose exec -T app /brainiac healthcheck >/dev/null 2>&1; then
		ok=1
		break
	fi
	sleep 2
done

if [ "$ok" = "1" ]; then
	echo "update: healthy ✓ (was $PREV_VERSION → now ${BRAINIAC_VERSION:-latest})"
	exit 0
fi

# 4. Failed to come up — print the rollback recipe. Do NOT auto-roll-back: the
# operator should decide, and a half-applied migration may need inspection first.
latest_snap="$(ls -1t "$SNAPSHOT_DIR"/brainiac-*.sql.gz 2>/dev/null | head -1 || true)"
{
	echo "update: app did NOT become healthy after the update."
	echo "roll back with:"
	echo "  1. pin the previous version:   BRAINIAC_VERSION=$PREV_VERSION (in .env)"
	echo "  2. restore the pre-migrate DB:  ./scripts/restore.sh --force ${latest_snap:-$SNAPSHOT_DIR/<newest>.sql.gz}"
	echo "  3. bring it back up:            docker compose up -d"
} >&2
exit 1
