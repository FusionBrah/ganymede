# StreamVault integration (`internal/streamvault`)

StreamVault-specific code, kept **isolated** from upstream Ganymede so this
fork stays trivial to rebase against the original
([github.com/Zibbp/ganymede](https://github.com/Zibbp/ganymede)).

Everything lives in this package. The only edit to upstream code is a single
`streamvault.Notify(...)` call in each branch of `checkIfTasksAreDone`
(`internal/tasks/shared.go`) — a two-line footprint that is easy to re-apply
after a `git sync`.

## What it does

When a VOD or live archive finishes, Ganymede POSTs a **signed
`archive.complete` webhook** to the StreamVault website. The website verifies
the signature and upserts the VOD into Supabase. Ganymede never learns what
Supabase is — the fork stays storage/DB-agnostic.

## Configuration (environment)

| Variable | Required | Purpose |
|---|---|---|
| `STREAMVAULT_WEBHOOK_URL` | yes | Website endpoint that receives the event |
| `STREAMVAULT_WEBHOOK_SECRET` | yes | Shared secret used to HMAC-sign the body |
| `CDN_URL` | no | Existing Ganymede var; echoed back so the receiver can build absolute media URLs |

The integration is a **no-op unless both URL and secret are set** — it fails
closed rather than emit events a receiver cannot authenticate. (URL set but
secret missing logs one warning and stays disabled.)

## Events

| Event | Fires when |
|---|---|
| `video.archived` | a past-broadcast/VOD archive completes |
| `live.archived` | a live-stream archive completes |

## Request

```
POST <STREAMVAULT_WEBHOOK_URL>
Content-Type: application/json
X-StreamVault-Event: video.archived        # or live.archived
X-StreamVault-Delivery: <uuid>             # unique per delivery; use for idempotency
X-StreamVault-Timestamp: <unix seconds>
X-StreamVault-Signature: sha256=<hex hmac>
```

Delivery is **at-least-once** (up to 3 attempts, linear backoff). Dedupe on
`X-StreamVault-Delivery`.

### Body

```json
{
  "event": "video.archived",
  "delivery_id": "b3f1c0de-2b1a-4c9e-8f77-0a1b2c3d4e5f",
  "timestamp": "2026-07-13T11:30:00Z",
  "cdn_url": "https://cdn.streamvault.gg",
  "channel": {
    "id": "8c4e0e2a-...",
    "ext_id": "12345",
    "name": "somestreamer",
    "display_name": "SomeStreamer",
    "image_path": "somestreamer/profile.png"
  },
  "vod": {
    "id": "0b9d...",
    "ext_id": "v98765",
    "platform": "twitch",
    "type": "archive",
    "title": "Ranked grind",
    "duration": 7200,
    "views": 4200,
    "resolution": "best",
    "video_path": "somestreamer/v98765/video.mp4",
    "chat_path": "somestreamer/v98765/chat.json",
    "chat_video_path": "somestreamer/v98765/chat.mp4",
    "thumbnail_path": "somestreamer/v98765/thumbnail.jpg",
    "web_thumbnail_path": "somestreamer/v98765/thumbnail-web.jpg",
    "info_path": "somestreamer/v98765/info.json",
    "folder_name": "v98765",
    "file_name": "video",
    "streamed_at": "2026-07-13T10:00:00Z",
    "created_at": "2026-07-13T11:29:00Z"
  },
  "queue": { "id": "3f2b...", "live_archive": false }
}
```

Path fields are **relative** to `CDN_URL` (or Ganymede's `VIDEOS_DIR`). Build a
playback URL as `` `${cdn_url}/${video_path}` ``.

## Verifying the signature (Cloudflare Workers / Web Crypto)

The signature is `HMAC-SHA256(secret, "<timestamp>.<rawBody>")`, hex-encoded.
Verify against the **raw** request body (do not re-serialize the parsed JSON).

```ts
export async function verifyStreamVaultWebhook(
  req: Request,
  rawBody: string,
  secret: string,
): Promise<boolean> {
  const ts = req.headers.get("X-StreamVault-Timestamp");
  const sig = req.headers.get("X-StreamVault-Signature");
  if (!ts || !sig) return false;

  // Reject stale deliveries to bound replay (5-minute window).
  const age = Math.abs(Date.now() / 1000 - Number(ts));
  if (!Number.isFinite(age) || age > 300) return false;

  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const mac = await crypto.subtle.sign(
    "HMAC",
    key,
    new TextEncoder().encode(`${ts}.${rawBody}`),
  );
  const expected =
    "sha256=" +
    [...new Uint8Array(mac)].map((b) => b.toString(16).padStart(2, "0")).join("");

  // Constant-time compare.
  if (expected.length !== sig.length) return false;
  let diff = 0;
  for (let i = 0; i < expected.length; i++) {
    diff |= expected.charCodeAt(i) ^ sig.charCodeAt(i);
  }
  return diff === 0;
}
```

## Tests

```
go test ./internal/streamvault/...
```

Covers payload assembly, the signed wire format, signed delivery against an
`httptest` server, and retry/exhaustion behaviour.
