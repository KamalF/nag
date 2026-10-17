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

// PushSender is the one send-and-handle helper that the sweep's digest and
// POST /api/push/test (§7.5) share. It holds the one copy of the §7.1
// response ladder, so there is no second copy to drift.
type PushSender struct {
	store        *store.Store
	log          *slog.Logger
	vapidPublic  string
	vapidPrivate string
	subject      string

	// sendFn is webpush.SendNotificationWithContext. It is injectable so
	// that tests can try every step of the ladder without a push service.
	sendFn func(ctx context.Context, payload []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error)
}

// recordSize is the encrypted body's exact length: webpush-go pads every
// push up to its record size, 4096 bytes by default. Mozilla's push service
// caps what it forwards at 4096 bytes. For Firefox on Android (routed
// through FCM), it counts the base64 body inside a JSON map that also holds
// a channel id and the encoding. So it refuses even a 3070-byte record with
// a 413. 2560 bytes encode to 3416, which leaves 680 bytes for that map.
// A 2560-byte record holds 2457 bytes of plaintext. That is more than the 1000-byte text
// of §8.3 can grow to, even with every byte JSON-escaped.
const recordSize = 2560

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
// "mailto:" to any Subscriber that does not start with "https:". §5.1
// requires the full URL in the env. If a mailto subject goes through as-is,
// the library signs sub = "mailto:mailto:…". That is not an RFC 8292
// contact, and a strict push service can answer it with a 403 on every
// send. The library's test is case-sensitive, but the §5.1 boot check
// (url.Parse) is not. So this function normalizes the case of the scheme.
// It does not assume that the scheme is lowercase.
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

// pushClient is the client that every push send goes through (§7.1). It
// dials only public addresses, checked after DNS. It never follows a
// redirect, so a 3xx lands on the ladder as http 3xx. It uses no proxy,
// because a proxy makes the dialed address the proxy's.
var pushClient = &http.Client{
	Transport: func() *http.Transport {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		t.DialContext = (&net.Dialer{Control: refuseNonPublic}).DialContext
		return t
	}(),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// errNonPublicAddress is the dial refusal. It classifies as refused.
var errNonPublicAddress = errors.New("push endpoint resolves to a non-public address")

// sharedAddressSpace is 100.64.0.0/10 (RFC 6598): carrier-grade NAT, and
// the range that Tailscale hands out. It is private in practice, but not to
// netip.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// refuseNonPublic is the dialer's Control hook. address is the resolved
// ip:port that the dialer is about to connect to.
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

// deleteTimeout bounds a deletion whose caller has no deadline of its own.
// The sweep runs on a context that never expires (§7.3).
const deleteTimeout = 5 * time.Second

// SendResult is the outcome of one attempt, in the closed vocabulary of
// §8.2. Status 0 means that the attempt produced no HTTP status at all
// (rendered as null). Deleted says whether this call dropped the row. Err is
// empty on success, or one of {timeout, refused, dns, http <code>, send
// failed, stale vapid key}.
type SendResult struct {
	Status  int
	Deleted bool
	Err     string
}

// SendTo sends payload to one subscription and applies the §7.1 ladder.
// 404/410 delete the row. 401/403 delete it only when the row's key differs
// from the configured one, because a subscription bound to a retired
// keypair can never succeed again. Otherwise, 401/403 are an ERROR and the
// row is kept. Any other status is logged and the row is kept. Endpoints
// are logged in truncated form only (§10.4). Success records nothing (§7.1).
//
// The two deadlines are separate on purpose. perSend bounds this one send:
// 10 s from the sweep, 3 s from the push test. It is the only part of a send
// that differs between the two callers. ctx carries whatever bounds the
// caller as a whole. For the push test, that is its 12-second budget (§7.5).
// The row deletion below uses ctx, not the context of the send. So the
// server still acts on a 410 that arrives in the last millisecond of a send.
// But the deletion never runs past a budget that the caller must answer
// within.
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

// deleteSubscription drops the row that the push service just declared
// dead, and reports whether the row went. `deleted` in the §7.5 response
// says what this call did, not what it intended. A caller that claims a row
// is gone while it is still in the table sends the reader to look in the
// wrong place.
//
// It runs on a context detached from the send's deadline. The response that
// proves the row is dead often arrives with almost nothing left of the
// budget. In §7.5, a send has only 3 seconds. If the expiry of the send
// cancels the delete, the row fails on every sweep forever. The ladder
// exists to stop exactly that (§7.1).
func (p *PushSender) deleteSubscription(ctx context.Context, endpoint, truncated string, status int) bool {
	// ctx here is the caller's, not the send's, so the expiry of a send
	// cannot cancel the write that acts on its answer. The extra bound keeps
	// a caller that has no deadline of its own (the sweep) from waiting
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

// classifySendError maps a transport failure onto the closed set of §4.1 by
// type only: errors.As down the chain, never text parsing. Anything that
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
