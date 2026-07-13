# Running this fork as the StreamVault backend

This repository is a fork of [Ganymede](https://github.com/Zibbp/ganymede) used
as **StreamVault's headless capture backend**. Ganymede does the Twitch VOD /
live archiving; the [StreamVault website](https://github.com/FusionBrah/StreamVault-website)
(Astro + Cloudflare Workers + Supabase) is the only client-facing surface.
Ganymede itself is never exposed to end users.

StreamVault-specific additions are kept **isolated** so this fork stays easy to
rebase against upstream ("the OG"):

- `internal/streamvault/` — signed `archive.complete` webhook (see its README).
- `scripts/` — Backblaze B2 storage sync.
- This file.

The only edit to upstream Go code is a two-line `streamvault.Notify(...)` call
in `internal/tasks/shared.go`.

## Architecture

```
                    Authorization: Bearer gym_…            signed webhook
   Website  ─────────────  REST  ─────────────▶  Ganymede  ───────────────▶  Website
 (dashboard)   provision channels / trigger      (this fork)  archive.complete   /api/webhooks
                    archives, read status                                    │  verify + upsert
                                                                             ▼
                                                                          Supabase

   Ganymede /data/videos ──rclone (completed VODs)──▶ Backblaze B2 ──▶ Cloudflare CDN ──▶ users
                          Ganymede retention prunes local after sync
```

## 1. API access (website → backend)

The website calls Ganymede with a scoped API key.

1. In Ganymede's admin UI, mint a key (**Admin > API Keys**). Keys are
   `gym_`-prefixed and shown once.
2. Grant the minimum scopes: `archive:write` + `live:write` + `channel:write`
   (add `queue:read` to poll job status). Avoid `*:admin`.
3. Give the website the key as `GANYMEDE_API_KEY` and send it as
   `Authorization: Bearer <key>` (tracked for the client in
   [website issue #114](https://github.com/FusionBrah/StreamVault-website/issues/114)).

API keys must be enabled in Ganymede's config (`api_keys_enabled`, default on).

## 2. Archive-complete webhook (backend → website)

On completion Ganymede POSTs a signed `video.archived` / `live.archived`
webhook to the website, which verifies the HMAC signature and upserts Supabase.
This is the event-driven replacement for the old `sync-to-b2.sh` Supabase cron.

Configure on the backend:

| Variable | Purpose |
|---|---|
| `STREAMVAULT_WEBHOOK_URL` | Website receiver endpoint |
| `STREAMVAULT_WEBHOOK_SECRET` | Shared HMAC secret (also set on the website) |

Full payload schema, headers, and a Cloudflare Workers verification snippet:
[`internal/streamvault/README.md`](internal/streamvault/README.md). The
integration is a no-op until both variables are set (fail-closed).

## 3. Storage (B2 + CDN + retention)

Completed VODs are synced to Backblaze B2 and served through Cloudflare; local
files are pruned by Ganymede's per-channel retention. Set `CDN_URL` to the
Cloudflare origin so playback URLs resolve to the CDN. Setup and the sync
script live in [`scripts/`](scripts/README.md).

## Environment reference (StreamVault-specific)

| Variable | Required | Description |
|---|---|---|
| `STREAMVAULT_WEBHOOK_URL` | for webhook | Website receiver endpoint |
| `STREAMVAULT_WEBHOOK_SECRET` | for webhook | Shared HMAC secret |
| `CDN_URL` | for CDN | Cloudflare/B2 base URL for static media (upstream Ganymede var) |

All other configuration is standard Ganymede (see the root `README.md`).

## Keeping this fork current with upstream

Because every StreamVault change is additive and isolated, syncing with
upstream stays clean:

```bash
git remote add upstream https://github.com/Zibbp/ganymede.git   # once
git fetch upstream
git rebase upstream/main    # re-apply the small streamvault delta
go build ./... && go test ./internal/streamvault/...
```

Keep it that way: new logic in its own package, wiring limited to one-line
calls at natural chokepoints, and never edit generated `ent/` code.
