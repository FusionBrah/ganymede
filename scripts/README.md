# StreamVault backend — storage scripts

Sync completed VOD storage from this Ganymede backend to **Backblaze B2**,
served to users through a **Cloudflare** CDN. Local files are pruned by
Ganymede's built-in per-channel retention.

```
Ganymede (/data/videos)  --rclone-->  Backblaze B2  <--cdn.streamvault.gg (Cloudflare)
        |   completed VODs only            |
        |   (processing = false)           +--> users stream from CDN
        +--> Ganymede retention prunes local after sync
```

VOD **metadata** is not synced here — the backend emits a signed
`archive.complete` webhook on completion (see `internal/streamvault`), which
the website turns into Supabase rows. These scripts are storage-only.

## How the sync decides what to upload

`sync-to-b2.sh` queries the Ganymede database for VODs where `processing =
false` and syncs only those `folder_name` directories. In-progress captures
are never uploaded. It works with either a direct PostgreSQL connection
(`DB_HOST` + `DB_PASS`) or, by default, `docker exec` into the `ganymede-db`
container.

## Setup

1. **Backblaze B2** — create a public bucket (e.g. `streamvault`) and an
   application key scoped to it.
2. **rclone** — `curl https://rclone.org/install.sh | sudo bash`, then
   `rclone config` to add a `b2` remote named `backblaze`. Verify with
   `rclone lsd backblaze:streamvault`.
3. **Cloudflare CDN** (free egress via the Bandwidth Alliance) — CNAME a
   `cdn` subdomain to your B2 endpoint (proxied / orange cloud), SSL mode
   Full (strict). Point Ganymede's `CDN_URL` at it.
4. **Install the script**
   ```bash
   sudo mkdir -p /opt/streamvault/scripts /var/log/streamvault
   sudo cp scripts/sync-to-b2.sh /opt/streamvault/scripts/
   sudo chmod +x /opt/streamvault/scripts/sync-to-b2.sh
   /opt/streamvault/scripts/sync-to-b2.sh   # test run (only syncs completed VODs)
   ```
5. **Cron** — `crontab scripts/crontab.example` (adjust paths first).
6. **Retention** — Admin > Channels > Edit > enable Retention, set days
   **longer** than the sync interval so files reach B2 before deletion.

## Environment

| Variable | Default | Description |
|---|---|---|
| `VIDEOS_DIR` | `/data/videos` | Local video storage |
| `B2_BUCKET` | `backblaze:streamvault/data/videos` | rclone `remote:bucket/path` |
| `LOG_DIR` | `/var/log/streamvault` | Log directory |
| `DB_CONTAINER` | `ganymede-db` | DB container (docker-exec fallback) |
| `DB_HOST` / `DB_PASS` | (empty) | Set both for a direct psql connection |
| `DB_PORT` / `DB_USER` / `DB_NAME` | `5432` / `ganymede` / `ganymede-prd` | DB connection |

## Troubleshooting

- **DB query fails** — set `DB_HOST` + `DB_PASS`, or confirm `docker ps`
  shows `ganymede-db`. Test: `docker exec ganymede-db psql -U ganymede -d ganymede-prd -c "SELECT count(*) FROM vods WHERE processing = false"`.
- **Sync not running** — a stale `/tmp/b2-sync.lock` means another run is in
  progress; check `tail -50 /var/log/streamvault/b2-sync.log`.
- **Files not syncing** — the VOD may still be processing
  (`SELECT folder_name, processing FROM vods ORDER BY created_at DESC LIMIT 10`).
