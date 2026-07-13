# Deploying the StreamVault backend

This fork runs like upstream Ganymede — two containers (app + Postgres) via
Docker Compose — with one crucial difference: **you must run an image built
from this fork**, because the StreamVault webhook (`internal/streamvault`) only
exists here.

> ⚠️ You must run an image built from **this fork** — upstream's
> `ghcr.io/zibbp/ganymede` has none of the webhook code, so the
> archive-complete webhook would never fire. The `docker-compose.yml` in this
> repo already points at the fork image (`ghcr.io/fusionbrah/ganymede:dev`,
> published by CI); step 3 explains where that image comes from.

For architecture and the webhook contract see
[`StreamVault-BACKEND.md`](StreamVault-BACKEND.md) and
[`internal/streamvault/README.md`](internal/streamvault/README.md).

## 1. Prerequisites

- Linux host with Docker + Docker Compose.
- A Twitch application — Client ID + Secret from <https://dev.twitch.tv/console/apps>.
- A storage path for VODs (local disk or mounted).
- Optional: the website's webhook URL, a shared secret, and a `CDN_URL` if
  serving media from B2.

## 2. Get the code on the host

```bash
git clone https://github.com/FusionBrah/ganymede.git
cd ganymede
```

## 3. Where the image comes from

CI is already configured to publish this fork to `ghcr.io/fusionbrah/ganymede`
(repo variable `OCI_PUSH=true`, workflow `.github/workflows/docker-build-publish.yml`):

- every push to `main` → `:dev` (rolling)
- a `vX.Y.Z` tag → `:X.Y.Z` (pinned)

`docker-compose.yml` references `:dev`. For a production deploy you may prefer
to pin a version:

```bash
git tag v0.1.0 && git push origin v0.1.0     # CI publishes :0.1.0
# then set image: ghcr.io/fusionbrah/ganymede:0.1.0 in docker-compose.yml
```

**First publish:** the image only exists once the workflow has run. Watch it in
the repo's **Actions** tab (or `gh run watch`). The GHCR package is created
**private** — make it public (GitHub → Packages → the package → Package
settings → Change visibility) or `docker login ghcr.io` on the host, so
`docker compose pull` can fetch it.

### Alternative — build from source on the host

No registry access needed: replace the app service's `image:` line with
`build: .` and use `docker compose up -d --build` in step 5. This builds the
fork's `Dockerfile` locally.

## 4. Configure `docker-compose.yml`

Minimum required edits:

- `TWITCH_CLIENT_ID`, `TWITCH_CLIENT_SECRET`
- `DB_PASS` **and** `POSTGRES_PASSWORD` — change both from `PASSWORD`
- `TZ`
- Volume `/path/to/vod/storage:/data/videos` → your real storage path
- Ports (default maps the app to `4800`, Postgres to `4801`)

Add the StreamVault webhook variables to the app service's `environment:` list
(they are **not** in the stock file):

```yaml
      - STREAMVAULT_WEBHOOK_URL=https://streamvault.gg/api/webhooks/ganymede
      - STREAMVAULT_WEBHOOK_SECRET=<long-random-secret>   # identical value on the website
      - CDN_URL=https://cdn.streamvault.gg                # only if using B2/CDN
```

The webhook is a **no-op until both `STREAMVAULT_WEBHOOK_URL` and
`STREAMVAULT_WEBHOOK_SECRET` are set** (fail-closed), so it is safe to deploy
without them and turn it on later.

## 5. First boot

```bash
docker compose pull && docker compose up -d   # published image
# docker compose up -d --build                # or build from source
docker compose logs -f ganymede               # watch startup + DB migrations
```

- Wait for the healthcheck (`/health` returns `OK`).
- Visit `http://<host>:4800`, log in with `admin` / `ganymede`, and **change
  the admin password immediately**.

## 6. Wire up the website

1. Admin → **API Keys** → create a key. Scopes: `archive:write`, `live:write`,
   `channel:write` (add `queue:read` to poll job status). Copy the `gym_…` key —
   it is shown once.
2. Give the website `GANYMEDE_API_URL` (`http://<host>:4800/api/v1`) and
   `GANYMEDE_API_KEY`.
3. Ensure the website's `STREAMVAULT_WEBHOOK_SECRET` matches step 4.

## 7. Verify the webhook

Point `STREAMVAULT_WEBHOOK_URL` at the real receiver (or a temporary request
bin), archive a short VOD, and confirm the worker log shows:

```
streamvault: webhook delivered   event=video.archived delivery_id=...
```

- `streamvault: webhook delivery failed` → the receiver returned ≥400 or is
  unreachable (it retries 3×).
- Nothing logged at all → one of the two env vars is unset.

## 8. B2 storage sync (optional)

Follow [`scripts/README.md`](scripts/README.md): configure `rclone`, install
`scripts/sync-to-b2.sh`, add the cron from `scripts/crontab.example`, and enable
per-channel Retention (Admin → Channels → Edit) with days **longer** than the
sync interval so files reach B2 before local deletion.

## 9. Updating & staying current with upstream

```bash
git checkout main && git pull
git fetch upstream && git rebase upstream/main   # if an 'upstream' remote is set
git push origin main                             # CI republishes :dev
docker compose pull && docker compose up -d      # roll the deploy
```

StreamVault changes are isolated (`internal/streamvault/`, `scripts/`, a
two-line hook in `internal/tasks/shared.go`), so rebasing onto upstream stays
clean.

## Security checklist

- [ ] Changed `DB_PASS` / `POSTGRES_PASSWORD` from `PASSWORD`
- [ ] Changed the `admin` account password
- [ ] `STREAMVAULT_WEBHOOK_SECRET` is long, random, and matches the website
- [ ] Postgres port (`4801`) is not exposed to the public internet
- [ ] API key uses minimal scopes (no `*:admin`)
- [ ] GHCR package visibility is intentional (public, or host is authenticated)
```
