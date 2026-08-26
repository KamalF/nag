package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
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

	if gotOpts.TTL != 259200 || gotOpts.Topic != "nag" || gotOpts.Urgency != webpush.UrgencyHigh ||
		gotOpts.Subscriber != "mailto:kamal@example.net" || gotOpts.VAPIDPublicKey != configuredKey {
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
// caller's context, not the send's — a 410 that answers as its own deadline
// expires still has to be acted on, or the row fails on every sweep until
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
// push test that has spent its 12 seconds (§7.5) must not spend more on a
// delete, and what it reports is what happened — `deleted` says what the
// call did (§7.5), and the next sweep will retire the row.
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
