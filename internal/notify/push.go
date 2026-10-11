package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

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

// recordSize is the encrypted body's exact length: webpush-go pads every
// push up to its record size, 4096 bytes by default. Mozilla's push service
// caps the body at 4096 bytes after base64 encoding, so the default is
// refused with a 413 on every send. 3070 bytes encode to 4096. They leave
// 2967 bytes of plaintext, more than §8.3's 1000-byte text can grow to
// even with every byte JSON-escaped.
const recordSize = 3070

func NewPushSender(st *store.Store, log *slog.Logger, vapidPublic, vapidPrivate, subject string) *PushSender {
	return &PushSender{
		store:        st,
		log:          log,
		vapidPublic:  vapidPublic,
		vapidPrivate: vapidPrivate,
		subject:      librarySubscriber(subject),
		sendFn:       webpush.SendNotificationWithContext,
	}
}

// librarySubscriber adapts NAG_VAPID_SUBJECT to webpush-go, which prefixes
// "mailto:" to any Subscriber not starting with "https:". §5.1 requires the
// full URL in the env, so passed through as-is a mailto subject would sign
// sub = "mailto:mailto:…", which is not an RFC 8292 contact and which a
// strict push service may answer with a 403 on every send. The library's
// test is case-sensitive while the §5.1 boot check (url.Parse) is not, so
// the scheme is normalised here rather than trusted.
func librarySubscriber(subject string) string {
	scheme, rest, _ := strings.Cut(subject, ":")
	switch strings.ToLower(scheme) {
	case "mailto":
		return rest
	case "https":
		return "https:" + rest
	}
	return subject
}

// pushClient is the client every push send goes through (§7.1): it dials
// only public addresses, checked after DNS; it never follows a redirect, so
// a 3xx lands on the ladder as http 3xx; and it uses no proxy, which would
// make the dialled address the proxy's.
var pushClient = &http.Client{
	Transport: func() *http.Transport {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		t.DialContext = (&net.Dialer{Control: refuseNonPublic}).DialContext
		return t
	}(),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// errNonPublicAddress is the dial refusal; it classifies as refused.
var errNonPublicAddress = errors.New("push endpoint resolves to a non-public address")

// sharedAddressSpace is 100.64.0.0/10 (RFC 6598): carrier-grade NAT, and
// the range Tailscale hands out — private in practice, not to netip.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// refuseNonPublic is the dialer's Control hook: address is the resolved
// ip:port about to be connected to.
func refuseNonPublic(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return errNonPublicAddress
	}
	ip := addrPort.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || sharedAddressSpace.Contains(ip) {
		return errNonPublicAddress
	}
	return nil
}

// deleteTimeout bounds a deletion whose caller has no deadline of its own —
// the sweep runs on a context that never expires (§7.3).
const deleteTimeout = 5 * time.Second

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
// logged and kept. Endpoints are logged in truncated form only (§10.4);
// success records nothing (§7.1).
//
// The two deadlines are separate on purpose. perSend bounds this one send —
// 10 s from the sweep, 3 s from the push test, the only part of a send that
// differs between the two callers — while ctx carries whatever bounds the
// caller as a whole, which for the push test is its 12-second budget (§7.5).
// The row deletion below hangs off ctx rather than the send, so a 410 that
// answers in the last millisecond of a send still gets acted on, and never
// at the cost of a budget the caller has to answer within.
func (p *PushSender) SendTo(ctx context.Context, perSend time.Duration, payload []byte, sub store.Subscription) SendResult {
	sendCtx, cancelSend := context.WithTimeout(ctx, perSend)
	defer cancelSend()

	resp, err := p.sendFn(sendCtx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		TTL:             259200, // ~3 days, safe because Topic bounds the queue at one (§7.1)
		Subscriber:      p.subject,
		VAPIDPublicKey:  p.vapidPublic,
		VAPIDPrivateKey: p.vapidPrivate,
		Urgency:         webpush.UrgencyHigh,
		RecordSize:      recordSize,
		Topic:           "nag",      // constant: at most one push ever pending per device (§7.1)
		HTTPClient:      pushClient, // without it, webpush-go uses a bare http.Client
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
		return SendResult{
			Status:  status,
			Deleted: p.deleteSubscription(ctx, sub.Endpoint, endpoint, status),
			Err:     fmt.Sprintf("http %d", status),
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if sub.VAPIDPublic != p.vapidPublic {
			p.log.Warn("push subscription bound to a retired vapid key, deleting",
				"endpoint", endpoint, "status", status)
			return SendResult{
				Status:  status,
				Deleted: p.deleteSubscription(ctx, sub.Endpoint, endpoint, status),
				Err:     "stale vapid key",
			}
		}
		p.log.Error("push rejected under the current vapid key — a bug or a push-service problem, keeping the subscription",
			"endpoint", endpoint, "status", status)
		return SendResult{Status: status, Err: fmt.Sprintf("http %d", status)}
	default:
		p.log.Error("push send failed", "endpoint", endpoint, "status", status)
		return SendResult{Status: status, Err: fmt.Sprintf("http %d", status)}
	}
}

// deleteSubscription drops the row the push service just declared dead, and
// reports whether it went: `deleted` in §7.5's response says what this call
// did, not what it intended, and a caller that claims a row is gone while it
// is still in the table sends the reader looking in the wrong place.
//
// It runs on a context detached from the send's deadline. The response that
// proves the row is dead often arrives with almost nothing left of the
// budget — §7.5's whole budget is 3 seconds per send — and a delete
// cancelled by that expiry would leave the row to fail on every sweep
// forever, which is exactly what the ladder exists to stop (§7.1).
func (p *PushSender) deleteSubscription(ctx context.Context, endpoint, truncated string, status int) bool {
	// ctx here is the caller's, not the send's, so the expiry of a send
	// cannot cancel the write that acts on its answer. The extra bound keeps
	// a caller that has no deadline of its own — the sweep — from waiting
	// indefinitely on the single connection (§4.3).
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	if err := p.store.DeleteSubscription(ctx, endpoint); err != nil {
		p.log.Error("delete subscription", "endpoint", truncated, "error", err)
		return false
	}
	p.log.Info("subscription deleted", "endpoint", truncated, "status", status)
	return true
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
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, errNonPublicAddress):
		return "refused"
	default:
		return "send failed"
	}
}
