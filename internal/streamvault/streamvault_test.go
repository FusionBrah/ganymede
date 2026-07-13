package streamvault

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zibbp/ganymede/ent"
)

func TestConfigEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"both set", Config{WebhookURL: "https://x", WebhookSecret: "s"}, true},
		{"url only", Config{WebhookURL: "https://x"}, false},
		{"secret only", Config{WebhookSecret: "s"}, false},
		{"neither", Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Enabled(); got != tc.want {
				t.Fatalf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildPayload(t *testing.T) {
	chID, vodID, qID := uuid.New(), uuid.New(), uuid.New()
	streamedAt := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	ts := time.Date(2026, 7, 13, 11, 30, 0, 0, time.UTC)

	ch := &ent.Channel{ID: chID, ExtID: "12345", Name: "somestreamer", DisplayName: "SomeStreamer", ImagePath: "/data/videos/somestreamer/profile.png"}
	vod := &ent.Vod{
		ID: vodID, ExtID: "v98765", Platform: "twitch", Type: "archive",
		Title: "Ranked grind", Duration: 7200, Views: 4200, Resolution: "best",
		VideoPath: "somestreamer/v98765/video.mp4", ChatPath: "somestreamer/v98765/chat.json",
		StreamedAt: streamedAt,
	}
	q := &ent.Queue{ID: qID, LiveArchive: true}

	p := BuildPayload(EventLiveArchived, "deliver-1", ts, Config{CDNURL: "https://cdn.example"}, ch, vod, q)

	if p.Event != EventLiveArchived || p.Delivery != "deliver-1" {
		t.Fatalf("event/delivery mismatch: %+v", p)
	}
	if p.Timestamp != "2026-07-13T11:30:00Z" {
		t.Fatalf("timestamp = %q", p.Timestamp)
	}
	if p.CDNURL != "https://cdn.example" {
		t.Fatalf("cdn = %q", p.CDNURL)
	}
	if p.Channel.ID != chID.String() || p.Channel.Name != "somestreamer" || p.Channel.DisplayName != "SomeStreamer" {
		t.Fatalf("channel mismatch: %+v", p.Channel)
	}
	if p.Vod.ID != vodID.String() || p.Vod.ExtID != "v98765" || p.Vod.Platform != "twitch" || p.Vod.Type != "archive" {
		t.Fatalf("vod identity mismatch: %+v", p.Vod)
	}
	if p.Vod.Duration != 7200 || p.Vod.Views != 4200 || p.Vod.VideoPath != "somestreamer/v98765/video.mp4" {
		t.Fatalf("vod detail mismatch: %+v", p.Vod)
	}
	if p.Vod.StreamedAt != "2026-07-13T10:00:00Z" {
		t.Fatalf("streamed_at = %q", p.Vod.StreamedAt)
	}
	if p.Queue.ID != qID.String() || !p.Queue.LiveArchive {
		t.Fatalf("queue mismatch: %+v", p.Queue)
	}
}

func TestBuildPayloadNilEntities(t *testing.T) {
	p := BuildPayload(EventVideoArchived, "d", time.Time{}, Config{}, nil, nil, nil)
	if p.Timestamp != "" {
		t.Fatalf("zero time should render empty, got %q", p.Timestamp)
	}
	if p.Channel.ID != "" || p.Vod.ID != "" || p.Queue.ID != "" {
		t.Fatalf("nil entities should yield empty ids: %+v", p)
	}
}

// TestSign pins the exact signed wire format ("<ts>.<body>") by recomputing
// the HMAC independently of Sign.
func TestSign(t *testing.T) {
	secret, ts, body := "topsecret", int64(1752399000), []byte(`{"event":"video.archived"}`)

	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(fmt.Sprintf("%d.", ts)))
	m.Write(body)
	want := hex.EncodeToString(m.Sum(nil))

	if got := Sign(secret, ts, body); got != want {
		t.Fatalf("Sign() = %s, want %s", got, want)
	}
	// Changing the body must change the signature.
	if Sign(secret, ts, []byte(`{}`)) == want {
		t.Fatal("signature did not change with body")
	}
	// Changing the timestamp must change the signature.
	if Sign(secret, ts+1, body) == want {
		t.Fatal("signature did not change with timestamp")
	}
}

func TestDeliverSignsAndPosts(t *testing.T) {
	secret := "shared-secret"

	type received struct {
		event, delivery, tsHeader, sig string
		body                           []byte
	}
	got := make(chan received, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- received{
			event:    r.Header.Get(HeaderEvent),
			delivery: r.Header.Get(HeaderDelivery),
			tsHeader: r.Header.Get(HeaderTimestamp),
			sig:      r.Header.Get(HeaderSignature),
			body:     b,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &deliverer{cfg: Config{WebhookURL: srv.URL, WebhookSecret: secret}, retry: time.Millisecond, http: srv.Client()}
	ts := int64(1752399000)
	body := []byte(`{"event":"video.archived","delivery_id":"abc"}`)

	if err := d.deliver(context.Background(), EventVideoArchived, "abc", ts, body); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	r := <-got
	if r.event != EventVideoArchived || r.delivery != "abc" {
		t.Fatalf("headers: event=%q delivery=%q", r.event, r.delivery)
	}
	if r.tsHeader != fmt.Sprintf("%d", ts) {
		t.Fatalf("timestamp header = %q", r.tsHeader)
	}
	if string(r.body) != string(body) {
		t.Fatalf("body altered in transit: %q", r.body)
	}
	// The receiver can verify the signature over "<ts>.<body>".
	wantSig := "sha256=" + Sign(secret, ts, body)
	if r.sig != wantSig {
		t.Fatalf("signature = %q, want %q", r.sig, wantSig)
	}
	// And it unmarshals back to a Payload.
	var p Payload
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("payload not valid json: %v", err)
	}
}

func TestDeliverRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &deliverer{cfg: Config{WebhookURL: srv.URL, WebhookSecret: "s"}, retry: time.Millisecond, http: srv.Client()}
	if err := d.deliver(context.Background(), EventVideoArchived, "id", 1, []byte(`{}`)); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("expected 3 attempts, got %d", n)
	}
}

func TestDeliverFailsAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	d := &deliverer{cfg: Config{WebhookURL: srv.URL, WebhookSecret: "s"}, retry: time.Millisecond, http: srv.Client()}
	if err := d.deliver(context.Background(), EventVideoArchived, "id", 1, []byte(`{}`)); err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if n := atomic.LoadInt32(&calls); n != maxAttempts {
		t.Fatalf("expected %d attempts, got %d", maxAttempts, n)
	}
}
