package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"syscall"

	"github.com/KamalF/nag/internal/store"
	webpush "github.com/SherClockHolmes/webpush-go"
)

// PushSender is the one send-and-handle helper shared by the sweep's
// digest and POST /api/push/test (§7.5) — one copy of the §7.1 response
// ladder, so there is no second one to drift.
type PushSender struct {
	store        *store.Store
	log          *slog.Logger
	vapidPublic  string
	vapidPrivate string
	subject      string

	// sendFn is webpush.SendNotificationWithContext, injectable so tests
	// can play every rung of the ladder without a push service.
	sendFn func(ctx context.Context, payload []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error)
}

func NewPushSender(st *store.Store, log *slog.Logger, vapidPublic, vapidPrivate, subject string) *PushSender {
	return &PushSender{
		store:        st,
		log:          log,
		vapidPublic:  vapidPublic,
		vapidPrivate: vapidPrivate,
		subject:      subject,
		sendFn:       webpush.SendNotificationWithContext,
	}
}

// SendResult is one attempt's outcome in §8.2's closed vocabulary:
// Status 0 means the attempt produced no HTTP status at all (rendered as
// null), Deleted says whether this call dropped the row, and Err is empty
// on success or one of {timeout, refused, dns, http <code>, send failed,
// stale vapid key}.
type SendResult struct {
	Status  int
	Deleted bool
	Err     string
}

// SendTo sends payload to one subscription and applies the §7.1 ladder:
// 404/410 delete the row; 401/403 delete only when the row's key differs
// from the configured one (a subscription bound to a retired keypair can
// never succeed again), and are an ERROR kept otherwise; anything else is
// logged and kept. The caller's ctx carries the deadline — 10 s from the
// sweep, min(3 s, remaining budget) from the push test — the only part of
// a send that differs between the two callers. Endpoints are logged in
// truncated form only (§10.4); success records nothing (§7.1).
func (p *PushSender) SendTo(ctx context.Context, payload []byte, sub store.Subscription) SendResult {
	resp, err := p.sendFn(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		TTL:             259200, // ~3 days, safe because Topic bounds the queue at one (§7.1)
		Subscriber:      p.subject,
		VAPIDPublicKey:  p.vapidPublic,
		VAPIDPrivateKey: p.vapidPrivate,
		Urgency:         webpush.UrgencyHigh,
		Topic:           "nag", // constant: at most one push ever pending per device (§7.1)
	})
	endpoint := TruncateEndpoint(sub.Endpoint)
	if err != nil {
		// the raw error routinely embeds the full endpoint URL, so only
		// the classified word reaches the log (§10.4)
		word := classifySendError(err)
		p.log.Error("push send failed", "endpoint", endpoint, "error", word)
		return SendResult{Err: word}
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	switch {
	case status < 300:
		return SendResult{Status: status}
	case status == http.StatusNotFound || status == http.StatusGone:
		p.deleteSubscription(ctx, sub.Endpoint, endpoint, status)
		return SendResult{Status: status, Deleted: true, Err: fmt.Sprintf("http %d", status)}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if sub.VAPIDPublic != p.vapidPublic {
			p.log.Warn("push subscription bound to a retired vapid key, deleted",
				"endpoint", endpoint, "status", status)
			p.deleteSubscription(ctx, sub.Endpoint, endpoint, status)
			return SendResult{Status: status, Deleted: true, Err: "stale vapid key"}
		}
		p.log.Error("push rejected under the current vapid key — a bug or a push-service problem, keeping the subscription",
			"endpoint", endpoint, "status", status)
		return SendResult{Status: status, Err: fmt.Sprintf("http %d", status)}
	default:
		p.log.Error("push send failed", "endpoint", endpoint, "status", status)
		return SendResult{Status: status, Err: fmt.Sprintf("http %d", status)}
	}
}

func (p *PushSender) deleteSubscription(ctx context.Context, endpoint, truncated string, status int) {
	if err := p.store.DeleteSubscription(ctx, endpoint); err != nil {
		p.log.Error("delete subscription", "endpoint", truncated, "error", err)
		return
	}
	p.log.Info("subscription deleted", "endpoint", truncated, "status", status)
}

// classifySendError maps a transport failure onto §4.1's closed set by
// type only — errors.As down the chain, never text parsing. Anything that
// cannot be typed is "send failed": a floor, not a fallback.
func classifySendError(err error) string {
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return "timeout" // attempted, did not complete (§7.2)
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	default:
		return "send failed"
	}
}
