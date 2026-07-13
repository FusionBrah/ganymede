#!/usr/bin/env bash
#
# StreamVault backend -> Backblaze B2 storage sync.
#
# Syncs only COMPLETED VOD folders (vods.processing = false) from the local
# Ganymede videos directory to Backblaze B2 via rclone. In-progress captures
# are never uploaded.
#
# This script is storage-only. VOD *metadata* is delivered to the StreamVault
# website by the backend's signed archive.complete webhook (see
# internal/streamvault), so — unlike the old embedded fork — there is no
# Supabase sync here.
#
# Local cleanup is handled by Ganymede's per-channel retention feature
# (Admin > Channels > Edit > Retention), not by this script. Set retention
# days LONGER than the sync interval so files reach B2 before deletion.

set -euo pipefail

VIDEOS_DIR="${VIDEOS_DIR:-/data/videos}"
B2_BUCKET="${B2_BUCKET:-backblaze:streamvault/data/videos}"
LOG_DIR="${LOG_DIR:-/var/log/streamvault}"
LOCK_FILE="${LOCK_FILE:-/tmp/b2-sync.lock}"

# Ganymede database connection. Direct psql is used when DB_HOST and DB_PASS
# are set; otherwise the script falls back to `docker exec` on the DB
# container. Defaults match the docker-compose 'ganymede-db' service.
DB_CONTAINER="${DB_CONTAINER:-ganymede-db}"
DB_HOST="${DB_HOST:-}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-ganymede}"
DB_PASS="${DB_PASS:-}"
DB_NAME="${DB_NAME:-ganymede-prd}"

mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/b2-sync.log"
INCLUDE_FILE="$(mktemp "${TMPDIR:-/tmp}/streamvault-sync-include.XXXXXX")"
trap 'rm -f "$INCLUDE_FILE"' EXIT

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG_FILE"; }

# Single-instance guard so a slow sync never overlaps the next cron tick.
exec 200>"$LOCK_FILE"
if ! flock -n 200; then
	log "sync already running, exiting"
	exit 0
fi

run_query() {
	local query="$1"
	if [ -n "$DB_HOST" ] && [ -n "$DB_PASS" ]; then
		PGPASSWORD="$DB_PASS" psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -t -A -c "$query"
	else
		docker exec "$DB_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -t -A -c "$query"
	fi
}

log "=== B2 sync starting: $VIDEOS_DIR -> $B2_BUCKET ==="

# Folder names of fully-processed VODs. folder_name is the per-VOD directory
# that holds the video, chat, thumbnails and sprites.
if ! FOLDERS=$(run_query "SELECT DISTINCT folder_name FROM vods WHERE processing = false AND folder_name IS NOT NULL AND folder_name != ''"); then
	log "ERROR: database query failed"
	log "  set DB_HOST + DB_PASS for a direct connection, or ensure container '$DB_CONTAINER' is running"
	exit 1
fi

: >"$INCLUDE_FILE"
folder_count=0
while IFS= read -r folder; do
	[ -n "$folder" ] || continue
	# Match the completed VOD folder anywhere under a channel directory.
	echo "*/$folder/**" >>"$INCLUDE_FILE"
	folder_count=$((folder_count + 1))
done <<<"$FOLDERS"

if [ "$folder_count" -eq 0 ]; then
	log "no completed VODs to sync"
	exit 0
fi
log "syncing $folder_count completed VOD folder(s)"

rclone sync "$VIDEOS_DIR" "$B2_BUCKET" \
	--include-from "$INCLUDE_FILE" \
	--exclude "*" \
	--transfers 4 \
	--checkers 8 \
	--log-file "$LOG_FILE" \
	--log-level INFO \
	--stats 1m \
	--stats-one-line

log "=== B2 sync complete ==="
