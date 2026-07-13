// Package streamvault holds StreamVault-specific integration that is kept
// deliberately isolated from upstream Ganymede so this fork stays trivial to
// rebase against the original (github.com/Zibbp/ganymede).
//
// It emits a signed "archive complete" webhook to the StreamVault website
// when a VOD or live archive finishes. The only wiring into upstream code is
// a single call to Notify from internal/tasks/shared.go on the
// archive-completion path — everything else (config, payload shape, signing,
// delivery) lives in this package.
//
// Configuration is read from the environment:
//
//	STREAMVAULT_WEBHOOK_URL     destination endpoint on the website
//	STREAMVAULT_WEBHOOK_SECRET  shared secret used to HMAC-sign the payload
//	CDN_URL                     (shared with upstream) base URL echoed back so
//	                            the receiver can build absolute media URLs
//
// The integration is a no-op unless both URL and secret are set — we fail
// closed rather than emit events a receiver cannot authenticate.
package streamvault

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/zibbp/ganymede/ent"
)

// Event types emitted to the StreamVault webhook.
const (
	EventVideoArchived = "video.archived"
	EventLiveArchived  = "live.archived"
)

// Outbound request headers. The signature covers "<timestamp>.<body>" so a
// receiver can reject stale (replayed) deliveries by bounding the timestamp.
const (
	HeaderEvent     = "X-StreamVault-Event"
	HeaderDelivery  = "X-StreamVault-Delivery"
	HeaderTimestamp = "X-StreamVault-Timestamp"
	HeaderSignature = "X-StreamVault-Signature"
)

const (
	maxAttempts    = 3
	defaultRetry   = 2 * time.Second
	defaultTimeout = 15 * time.Second
)

// warnOnce guards a single misconfiguration warning so a busy archive queue
// doesn't spam the log on every completion.
var warnOnce sync.Once

// Config holds the StreamVault webhook settings read from the environment.
type Config struct {
	WebhookURL    string
	WebhookSecret string
	CDNURL        string
}

// configFromEnv reads the StreamVault webhook configuration. CDN_URL is
// shared with upstream Ganymede's static-file serving.
func configFromEnv() Config {
	return Config{
		WebhookURL:    os.Getenv("STREAMVAULT_WEBHOOK_URL"),
		WebhookSecret: os.Getenv("STREAMVAULT_WEBHOOK_SECRET"),
		CDNURL:        os.Getenv("CDN_URL"),
	}
}

// Enabled reports whether the integration is fully configured. Both a URL and
// a secret are required; a URL without a secret is treated as disabled.
func (c Config) Enabled() bool {
	return c.WebhookURL != "" && c.WebhookSecret != ""
}

// Payload is the JSON body delivered to the StreamVault webhook.
type Payload struct {
	Event     string         `json:"event"`
	Delivery  string         `json:"delivery_id"`
	Timestamp string         `json:"timestamp"`
	CDNURL    string         `json:"cdn_url,omitempty"`
	Channel   ChannelPayload `json:"channel"`
	Vod       VodPayload     `json:"vod"`
	Queue     QueuePayload   `json:"queue"`
}

// ChannelPayload is the channel subset the website needs to upsert a channel.
type ChannelPayload struct {
	ID          string `json:"id"`
	ExtID       string `json:"ext_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	ImagePath   string `json:"image_path"`
}

// VodPayload is the VOD subset the website needs to upsert a video and build
// playback/download URLs (paths are relative to CDN_URL / VIDEOS_DIR).
type VodPayload struct {
	ID               string `json:"id"`
	ExtID            string `json:"ext_id"`
	Platform         string `json:"platform"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	Duration         int    `json:"duration"`
	Views            int    `json:"views"`
	Resolution       string `json:"resolution"`
	VideoPath        string `json:"video_path"`
	ChatPath         string `json:"chat_path,omitempty"`
	ChatVideoPath    string `json:"chat_video_path,omitempty"`
	ThumbnailPath    string `json:"thumbnail_path,omitempty"`
	WebThumbnailPath string `json:"web_thumbnail_path,omitempty"`
	InfoPath         string `json:"info_path,omitempty"`
	FolderName       string `json:"folder_name,omitempty"`
	FileName         string `json:"file_name,omitempty"`
	StreamedAt       string `json:"streamed_at"`
	CreatedAt        string `json:"created_at"`
}

// QueuePayload carries the originating queue item for correlation.
type QueuePayload struct {
	ID          string `json:"id"`
	LiveArchive bool   `json:"live_archive"`
}

// BuildPayload assembles the webhook payload from the archive entities. Nil
// entities yield zero-valued sub-objects rather than panicking.
func BuildPayload(event, delivery string, ts time.Time, cfg Config, ch *ent.Channel, vod *ent.Vod, q *ent.Queue) Payload {
	p := Payload{
		Event:     event,
		Delivery:  delivery,
		Timestamp: formatTime(ts),
		CDNURL:    cfg.CDNURL,
	}
	if ch != nil {
		p.Channel = ChannelPayload{
			ID:          ch.ID.String(),
			ExtID:       ch.ExtID,
			Name:        ch.Name,
			DisplayName: ch.DisplayName,
			ImagePath:   ch.ImagePath,
		}
	}
	if vod != nil {
		p.Vod = VodPayload{
			ID:               vod.ID.String(),
			ExtID:            vod.ExtID,
			Platform:         string(vod.Platform),
			Type:             string(vod.Type),
			Title:            vod.Title,
			Duration:         vod.Duration,
			Views:            vod.Views,
			Resolution:       vod.Resolution,
			VideoPath:        vod.VideoPath,
			ChatPath:         vod.ChatPath,
			ChatVideoPath:    vod.ChatVideoPath,
			ThumbnailPath:    vod.ThumbnailPath,
			WebThumbnailPath: vod.WebThumbnailPath,
			InfoPath:         vod.InfoPath,
			FolderName:       vod.FolderName,
			FileName:         vod.FileName,
			StreamedAt:       formatTime(vod.StreamedAt),
			CreatedAt:        formatTime(vod.CreatedAt),
		}
	}
	if q != nil {
		p.Queue = QueuePayload{
			ID:          q.ID.String(),
			LiveArchive: q.LiveArchive,
		}
	}
	return p
}

// Sign returns the hex-encoded HMAC-SHA256 of "<ts>.<body>" using secret.
// The timestamp is bound into the signature so replays can be rejected.
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// formatTime renders a time as RFC3339 (UTC), or "" for the zero time.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Notify delivers an archive-complete event to the configured StreamVault
// webhook. It is a non-blocking no-op when the integration is not configured,
// and spawns its own panic-guarded goroutine, so callers can invoke it
// directly on the archive-completion path with a single line.
func Notify(ctx context.Context, event string, ch *ent.Channel, vod *ent.Vod, q *ent.Queue) {
	cfg := configFromEnv()
	if !cfg.Enabled() {
		if cfg.WebhookURL != "" && cfg.WebhookSecret == "" {
			warnOnce.Do(func() {
				log.Warn().Msg("streamvault: STREAMVAULT_WEBHOOK_URL set but STREAMVAULT_WEBHOOK_SECRET missing; webhooks disabled")
			})
		}
		return
	}

	delivery := uuid.NewString()
	ts := time.Now()
	body, err := json.Marshal(BuildPayload(event, delivery, ts, cfg, ch, vod, q))
	if err != nil {
		log.Error().Err(err).Str("event", event).Msg("streamvault: error marshalling webhook payload")
		return
	}

	// Detach from the request context so a cancellation once archiving
	// completes doesn't abort an in-flight delivery.
	deliverCtx := context.WithoutCancel(ctx)
	d := &deliverer{cfg: cfg, retry: defaultRetry, http: &http.Client{Timeout: defaultTimeout}}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Msg("streamvault: panic in webhook delivery")
			}
		}()
		if err := d.deliver(deliverCtx, event, delivery, ts.Unix(), body); err != nil {
			log.Error().Err(err).Str("event", event).Str("delivery_id", delivery).Msg("streamvault: webhook delivery failed")
			return
		}
		log.Debug().Str("event", event).Str("delivery_id", delivery).Msg("streamvault: webhook delivered")
	}()
}

// deliverer performs a signed POST with bounded retries.
type deliverer struct {
	cfg   Config
	retry time.Duration
	http  *http.Client
}

// deliver signs and POSTs body, retrying transient failures (network errors
// or >=400 responses) up to maxAttempts with linear backoff.
func (d *deliverer) deliver(ctx context.Context, event, delivery string, ts int64, body []byte) error {
	sig := Sign(d.cfg.WebhookSecret, ts, body)

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return fmt.Errorf("canceled before attempt %d: %w", attempt, ctx.Err())
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.WebhookURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("error creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderEvent, event)
		req.Header.Set(HeaderDelivery, delivery)
		req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
		req.Header.Set(HeaderSignature, "sha256="+sig)

		resp, err := d.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close() //nolint:errcheck
			if resp.StatusCode < 400 {
				return nil
			}
			lastErr = fmt.Errorf("received status %d", resp.StatusCode)
		}

		if attempt == maxAttempts {
			break
		}
		timer := time.NewTimer(d.retry * time.Duration(attempt))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("canceled while waiting to retry: %w", ctx.Err())
		case <-timer.C:
		}
	}

	return fmt.Errorf("failed after %d attempts: %w", maxAttempts, lastErr)
}
