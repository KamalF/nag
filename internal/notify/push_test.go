package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/KamalF/nag/internal/store"
	webpush "github.com/SherClockHolmes/webpush-go"
)

const (
	configuredKey = "configured-vapid-public"
	fullEndpoint  = "https://push.example.net/send/AbCdEfGh12345678"
)

func pushFixture(t *testing.T, subscriptionKey string) (*PushSender, *store.Store, *strings.Builder) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "nag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertSubscription(t.Context(), fullEndpoint, "p256dh", "auth", subscriptionKey, 1); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	p := NewPushSender(st, slog.New(slog.NewTextHandler(&logs, nil)),
		configuredKey, "vapid-private", "mailto:kamal@example.net")
	return p, st, &logs
}

func respondWith(status int) func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
	return func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
}

func theSubscription(t *testing.T, st *store.Store) store.Subscription {
	t.Helper()
	subs, err := st.ListSubscriptions(t.Context())
	if err != nil || len(subs) != 1 {
		t.Fatalf("subscriptions = %v (err %v), want one", subs, err)
	}
	return subs[0]
}

func subscriptionCount(t *testing.T, st *store.Store) int {
	t.Helper()
	n, err := st.SubscriptionCount(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSendToLadder(t *testing.T) {
	t.Run("success records nothing", func(t *testing.T) {
		p, st, logs := pushFixture(t, configuredKey)
		p.sendFn = respondWith(201)
		got := p.SendTo(t.Context(), time.Second, []byte("{}"), theSubscription(t, st))
		if got != (SendResult{Status: 201}) {
			t.Errorf("result = %+v, want bare 201", got)
		}
		if subscriptionCount(t, st) != 1 {
			t.Error("success deleted the row")
		}
		if logs.Len() != 0 {
			t.Errorf("success logged: %q", logs.String())
		}
	})

	for _, status := range []int{404, 410} {
		t.Run("gone endpoint deletes the row", func(t *testing.T) {
			p, st, _ := pushFixture(t, configuredKey)
			p.sendFn = respondWith(status)
			got := p.SendTo(t.Context(), time.Second, []byte("{}"), theSubscription(t, st))
			if !got.Deleted || got.Status != status {
				t.Errorf("result = %+v, want deleted at %d", got, status)
			}
			if subscriptionCount(t, st) != 0 {
				t.Errorf("%d did not delete the row", status)
			}
		})
	}

	t.Run("403 under a retired key deletes and warns", func(t *testing.T) {
		p, st, logs := pushFixture(t, "some-old-key")
		p.sendFn = respondWith(403)
		got := p.SendTo(t.Context(), time.Second, []byte("{}"), theSubscription(t, st))
		if !got.Deleted || got.Err != "stale vapid key" {
			t.Errorf("result = %+v, want deleted with \"stale vapid key\"", got)
		}
		if subscriptionCount(t, st) != 0 {
			t.Error("stale-key row survived")
		}
		if !strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("no WARNING logged: %q", logs.String())
		}
	})

	t.Run("403 under the current key is kept as an ERROR", func(t *testing.T) {
		p, st, logs := pushFixture(t, configuredKey)
		p.sendFn = respondWith(403)
		got := p.SendTo(t.Context(), time.Second, []byte("{}"), theSubscription(t, st))
		if got.Deleted || got.Err != "http 403" {
			t.Errorf("result = %+v, want kept with \"http 403\"", got)
		}
		if subscriptionCount(t, st) != 1 {
			t.Error("matching-key row was deleted — that branch is a bug report, not a cleanup")
		}
		if !strings.Contains(logs.String(), "level=ERROR") {
			t.Errorf("no ERROR logged: %q", logs.String())
		}
	})

	t.Run("other statuses are logged and kept", func(t *testing.T) {
		p, st, _ := pushFixture(t, configuredKey)
		p.sendFn = respondWith(500)
		got := p.SendTo(t.Context(), time.Second, []byte("{}"), theSubscription(t, st))
		if got.Deleted || got.Err != "http 500" {
			t.Errorf("result = %+v, want kept with \"http 500\"", got)
		}
		if subscriptionCount(t, st) != 1 {
			t.Error("500 deleted the row")
		}
	})
}

func TestSendToTransportClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"context deadline", context.DeadlineExceeded, "timeout"},
		{"wrapped deadline", &net.OpError{Op: "dial", Err: context.DeadlineExceeded}, "timeout"},
		{"dns", &net.DNSError{Name: "push.example.net", IsNotFound: true}, "dns"},
		{"refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "refused"},
		{"untypeable", errors.New("the library flattened everything into a string"), "send failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, st, _ := pushFixture(t, configuredKey)
			p.sendFn = func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
				return nil, tt.err
			}
			got := p.SendTo(t.Context(), time.Second, []byte("{}"), theSubscription(t, st))
			if got.Err != tt.want || got.Status != 0 || got.Deleted {
				t.Errorf("result = %+v, want {0 false %q}", got, tt.want)
			}
			if subscriptionCount(t, st) != 1 {
				t.Error("a transport failure deleted the row")
			}
		})
	}
}

// §7.1's pinned options, and §10.4's no-full-endpoint rule.
func TestSendToOptionsAndLogging(t *testing.T) {
	p, st, logs := pushFixture(t, "some-old-key")
	var gotOpts *webpush.Options
	var gotSub *webpush.Subscription
	p.sendFn = func(_ context.Context, _ []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error) {
		gotSub, gotOpts = sub, opts
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	p.SendTo(t.Context(), time.Second, []byte("{}"), theSubscription(t, st))

	if gotOpts.TTL != 259200 || gotOpts.Topic != "nag" || gotOpts.Urgency != webpush.UrgencyHigh || gotOpts.RecordSize != recordSize ||
		gotOpts.Subscriber != "kamal@example.net" || gotOpts.VAPIDPublicKey != configuredKey {
		t.Errorf("options = %+v, want the §7.1 pinned set", gotOpts)
	}
	if gotSub.Endpoint != fullEndpoint || gotSub.Keys.P256dh != "p256dh" || gotSub.Keys.Auth != "auth" {
		t.Errorf("subscription = %+v", gotSub)
	}

	line := logs.String()
	if strings.Contains(line, "/send/AbCdEfGh12345678") {
		t.Errorf("full endpoint leaked into the log: %q", line)
	}
	if !strings.Contains(line, "12345678") {
		t.Errorf("truncated endpoint form missing: %q", line)
	}
}

// §7.1's delete is the one write that cannot be skipped: the response is
// the only thing that ever knows the row is dead. So it hangs off the
// caller's context, not the send's. A 410 that answers as its own deadline
// expires must still be acted on. If not, the row fails on every sweep until
// another send happens to catch it earlier.
func TestDeleteSurvivesAnExhaustedSendDeadline(t *testing.T) {
	p, st, _ := pushFixture(t, configuredKey)
	p.sendFn = func(ctx context.Context, _ []byte, _ *webpush.Subscription, _ *webpush.Options) (*http.Response, error) {
		<-ctx.Done() // the send spends its whole deadline, then answers
		return &http.Response{StatusCode: http.StatusGone, Body: io.NopCloser(strings.NewReader(""))}, nil
	}

	got := p.SendTo(t.Context(), time.Millisecond, []byte("{}"), theSubscription(t, st))

	if !got.Deleted {
		t.Errorf("Deleted = false on a 410, want true")
	}
	if n := subscriptionCount(t, st); n != 0 {
		t.Errorf("%d subscriptions left, want the dead row gone", n)
	}
}

// The other half of that trade: the caller's own budget does bound it. A
// push test that spent its 12 seconds (§7.5) must not spend more on a
// delete. What it reports is what happened: `deleted` says what the call
// did (§7.5), and the next sweep will retire the row.
func TestDeleteRespectsAnExpiredCallerBudget(t *testing.T) {
	p, st, _ := pushFixture(t, configuredKey)
	p.sendFn = respondWith(http.StatusGone)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got := p.SendTo(ctx, time.Second, []byte("{}"), theSubscription(t, st))

	if got.Deleted {
		t.Errorf("Deleted = true past a spent budget, want false — it reports what it did")
	}
	if n := subscriptionCount(t, st); n != 1 {
		t.Errorf("%d subscriptions left, want the row still there for the next sweep", n)
	}
}

// browserSubscription is a subscription with real client keys, so a send
// to it is encrypted the way a browser's would be.
func browserSubscription(t *testing.T, endpoint, vapidPublic string) store.Subscription {
	t.Helper()
	client, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	rand.Read(authSecret)
	return store.Subscription{
		Endpoint:    endpoint,
		P256dh:      base64.RawURLEncoding.EncodeToString(client.PublicKey().Bytes()),
		Auth:        base64.RawURLEncoding.EncodeToString(authSecret),
		VAPIDPublic: vapidPublic,
	}
}

// Mozilla's push service answers 413 when a push that it forwards to FCM
// (Firefox on Android) is over 4096 bytes. It measures the base64 body
// inside a JSON map with a channel id, the encoding and a reliability id.
// webpush-go pads every body to the record size. So the largest payload
// that nag can build, a 1000-byte text of quotes that JSON doubles, must
// encrypt into a body that leaves room for that map.
func TestLargestPayloadFitsMozillaLimit(t *testing.T) {
	var bodyLen int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyLen = len(body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "nag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	payload, err := buildDigestPayload([]store.HeldRow{{
		ID: math.MaxInt64, DueAt: math.MaxInt64, Text: strings.Repeat(`"`, 1000),
	}})
	if err != nil {
		t.Fatal(err)
	}

	p := NewPushSender(st, slog.New(slog.NewTextHandler(io.Discard, nil)), public, private, "mailto:kamal@example.net")
	p.sendFn = func(ctx context.Context, b []byte, s *webpush.Subscription, o *webpush.Options) (*http.Response, error) {
		o.HTTPClient = srv.Client() // pushClient refuses loopback
		return webpush.SendNotificationWithContext(ctx, b, s, o)
	}
	if res := p.SendTo(t.Context(), 5*time.Second, payload, browserSubscription(t, srv.URL+"/send/abc", public)); res.Err != "" {
		t.Fatalf("send of a %d-byte payload failed: %+v", len(payload), res)
	}
	const fcmMapAllowance = 512 // {"chid":"<uuid>","body":…,"con":"aes128gcm","rid":…}
	if encoded := base64.StdEncoding.EncodedLen(bodyLen); encoded > 4096-fcmMapAllowance {
		t.Errorf("body is %d bytes, %d once base64 encoded, want at most %d", bodyLen, encoded, 4096-fcmMapAllowance)
	}
}

// The claim that goes out, not the option handed to the library. webpush-go
// adds its own "mailto:" to a non-https Subscriber. So the test must read
// the JWT's sub off a real request to know what a push service sees.
func TestVAPIDSubjectClaimIsOneMailto(t *testing.T) {
	var auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	sub := browserSubscription(t, srv.URL+"/send/abc", public)
	st, err := store.Open(filepath.Join(t.TempDir(), "nag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	for subject, want := range map[string]string{
		"mailto:kamal@example.net": "mailto:kamal@example.net",
		"https://nag.example.net":  "https://nag.example.net",
		"MAILTO:kamal@example.net": "mailto:kamal@example.net", // §5.1's check accepts it
		"HTTPS://nag.example.net":  "https://nag.example.net",
	} {
		t.Run(subject, func(t *testing.T) {
			p := NewPushSender(st, slog.New(slog.NewTextHandler(io.Discard, nil)), public, private, subject)
			// check that SendTo handed over pushClient, because webpush-go
			// replaces a missing client. Then do the real send through the
			// test server's own client, because pushClient refuses loopback.
			p.sendFn = func(ctx context.Context, b []byte, s *webpush.Subscription, o *webpush.Options) (*http.Response, error) {
				if o.HTTPClient != pushClient {
					t.Error("SendTo does not send through pushClient")
				}
				o.HTTPClient = srv.Client()
				return webpush.SendNotificationWithContext(ctx, b, s, o)
			}
			if res := p.SendTo(t.Context(), 5*time.Second, []byte(`{"n":1,"texts":["x"]}`), sub); res.Err != "" {
				t.Fatalf("send failed: %+v", res)
			}

			// Authorization: vapid t=<jwt>, k=<key>
			token := strings.TrimPrefix(strings.Split(auth, ",")[0], "vapid t=")
			parts := strings.Split(token, ".")
			if len(parts) != 3 {
				t.Fatalf("no JWT in %q", auth)
			}
			raw, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var claims struct{ Sub string }
			if err := json.Unmarshal(raw, &claims); err != nil {
				t.Fatal(err)
			}
			if claims.Sub != want {
				t.Errorf("sub claim = %q, want %q", claims.Sub, want)
			}
		})
	}
}

// A push endpoint can only reach a public address (§7.1). The dial refuses,
// after DNS, every address inside its own network that a token holder can
// aim the server at.
func TestRefuseNonPublic(t *testing.T) {
	for address, refused := range map[string]bool{
		"127.0.0.1:443":          true,
		"10.0.0.5:443":           true,
		"172.16.0.1:443":         true,
		"192.168.1.1:443":        true,
		"169.254.169.254:80":     true, // cloud metadata
		"100.100.100.100:443":    true, // Tailscale / CGNAT
		"0.0.0.0:443":            true,
		"[::1]:443":              true,
		"[fd00::1]:443":          true,
		"[fe80::1]:443":          true,
		"[::ffff:10.0.0.1]:443":  true,
		"142.250.74.106:443":     false, // fcm.googleapis.com
		"[2a00:1450:4007::]:443": false,
	} {
		err := refuseNonPublic("tcp", address, nil)
		if refused != errors.Is(err, errNonPublicAddress) {
			t.Errorf("refuseNonPublic(%s) = %v, want refused %v", address, err, refused)
		}
	}
}

// The guard is on the client that every send uses. A loopback endpoint is
// refused before a byte is sent, and is classified as refused. A redirect
// is returned as the response, not followed.
func TestPushClientStaysPublic(t *testing.T) {
	reached := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer srv.Close()
	_, err := pushClient.Post(srv.URL, "application/octet-stream", nil)
	if !errors.Is(err, errNonPublicAddress) || classifySendError(err) != "refused" || reached {
		t.Errorf("loopback endpoint: err %v, class %q, reached %v", err, classifySendError(err), reached)
	}
	if pushClient.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("the push client follows redirects")
	}
	if pushClient.Transport.(*http.Transport).Proxy != nil {
		t.Error("the push client uses a proxy, whose address the dial guard would check instead")
	}
}
