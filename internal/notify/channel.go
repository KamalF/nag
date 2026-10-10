package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nicholas-fedor/shoutrrr/pkg/router"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"
	"github.com/nicholas-fedor/shoutrrr/pkg/util"
	"github.com/nicholas-fedor/shoutrrr/pkg/util/jsonclient"
)

// ChannelSendTimeout is §7.2's per-channel bound, the same 10 s as a push.
// A caller's context may cut it shorter: phase 3's 20-second budget makes
// each send take the smaller of the two.
const ChannelSendTimeout = 10 * time.Second

// newRouter builds a shoutrrr router. The library's own logger is
// discarded: its lines routinely carry the URL it was given, and a channel
// URL is a secret that must never reach the log (§10.4).
func newRouter(opts types.SenderOptions) *router.ServiceRouter {
	r, _ := router.NewWithOptions(util.DiscardLogger, opts) // no URLs, so no error
	return r
}

// ValidateChannelURL is `nag channel add`'s parse-before-write (§7.4): the
// URL goes through shoutrrr's router exactly as a send would build it, so a
// typo is refused at the prompt rather than stored and found weeks later
// as a delivery_error.
func ValidateChannelURL(rawURL string) error {
	_, err := newRouter(types.SenderOptions{}).Locate(rawURL)
	return err
}

// SendChannel sends one message to one channel URL and returns "" on
// success or the §4.1 classification of the failure: timeout, refused,
// dns, http <code>, or send failed — never the library's text, which
// routinely embeds the URL.
//
// The bound is imposed here, because shoutrrr's API cannot carry it (§7.2):
// the send runs on its own goroutine into a buffered channel of one, and the
// caller selects on that and on a context with the 10-second timeout,
// derived from ctx so a shorter caller deadline wins. A context that
// expires first abandons the send — recorded as timeout, the word for
// "attempted, did not complete", whether a deadline or a shutdown ended it.
//
// §7.2 expects the abandoned goroutine to end on its own client's timeout.
// Most v0.21.2 services do bound themselves, but not all: pushbullet,
// opsgenie, notifiarr and generic fall back to a bare http.Client (generic
// gets a timed one injected, below). So the leak is bounded here, whatever
// the service does: while an earlier send to the same URL is still running
// — and has run for less than busyFor — no new one starts, and the verdict
// is NotAttempted. Nothing was attempted, so nothing is recorded for it
// (§4.1); the sweep logs the skip. A host that hangs forever then costs
// one goroutine per busyFor, not one per reminder, and the channel is
// tried again after that rather than muted for good.
//
// A service offering SendContext gets the context too — in v0.21.2, smtp
// and xmpp; smtp turns the context's deadline into one on its connection,
// so an abandoned smtp send now ends on its own.
func SendChannel(ctx context.Context, rawURL, message string) string {
	slot := &sendSlot{started: time.Now()}
	if prev, busy := inFlight.LoadOrStore(rawURL, slot); busy {
		if time.Since(prev.(*sendSlot).started) < busyFor {
			return NotAttempted
		}
		// stuck past any timeout a service could have: take the slot over
		inFlight.Store(rawURL, slot)
	}
	sendCtx, cancel := context.WithTimeout(ctx, ChannelSendTimeout)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		err := sendOnce(sendCtx, rawURL, message)
		// released before the verdict is handed over, so a caller that
		// sends again at once never finds its own finished send still busy;
		// and only this send's own slot, never one that took it over
		inFlight.CompareAndDelete(rawURL, slot)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			return ""
		}
		return classifyChannelError(err)
	case <-sendCtx.Done():
		return "timeout"
	}
}

// NotAttempted is SendChannel's verdict for a send it did not start because
// an earlier one to the same URL is still running. It is not a §4.1
// classification and never reaches delivery_error.
const NotAttempted = "not attempted"

// busyFor is how long a running send keeps new ones to its URL from
// starting: past every client timeout a v0.21.2 service sets (30 s is the
// longest), so only a send that will never end on its own is ever taken
// over.
var busyFor = 3 * ChannelSendTimeout

type sendSlot struct{ started time.Time }

// inFlight maps a channel URL to the send currently running for it,
// abandoned or not.
var inFlight sync.Map

// timedClientServices get an injected http.Client carrying the send
// timeout, so an abandoned send ends on its own and frees its inFlight slot
// instead of muting the channel until the host closes the socket. Only
// generic: it falls back to a bare &http.Client{} with
// context.Background(), and its SetHTTPClient only swaps the field. The
// router injects after a service has initialised, which elsewhere replaces
// a client the service configured itself — ntfy and gotify lose their TLS
// options (disabletlsverification, insecureskipverify), and pushbullet
// loses the Access-Token header it set on its own client — so injecting
// anywhere else breaks the sends it was meant to bound. Re-check when the
// pin moves.
var timedClientServices = map[string]bool{"generic": true}

// abandonedSendTimeout is the injected client's timeout; a variable so a
// test can shorten it.
var abandonedSendTimeout = ChannelSendTimeout

func sendOnce(ctx context.Context, rawURL, message string) error {
	var opts types.SenderOptions
	if scheme, _, err := newRouter(opts).ExtractServiceName(rawURL); err == nil && timedClientServices[scheme] {
		opts.HTTPClient = &http.Client{Timeout: abandonedSendTimeout}
	}
	service, err := newRouter(opts).Locate(rawURL)
	if err != nil {
		return err
	}
	if cs, ok := service.(types.ContextSender); ok {
		return cs.SendContext(ctx, message, nil)
	}
	return service.Send(message, nil)
}

// classifyChannelError reads the error's type and never its text (§4.1). A
// status appears only when the library surfaces it as a value — a code that
// exists only inside a sentence is not one, and anything that cannot be
// typed is "send failed": a floor, not a fallback. Only a failure status
// counts: jsonclient also returns its Error for a 2xx whose body is not
// JSON (an auth proxy's login page), and "http 200" would read as a
// contradiction in a column of failures.
func classifyChannelError(err error) string {
	if code := failureStatus(err); code != 0 {
		return fmt.Sprintf("http %d", code)
	}
	return classifySendError(err)
}

func failureStatus(err error) int {
	// jsonclient returns its Error by value — it has a value receiver
	var status jsonclient.Error
	if errors.As(err, &status) && status.StatusCode >= 400 {
		return status.StatusCode
	}
	return 0
}
