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

// ChannelSendTimeout is the per-channel bound of §7.2, the same 10 s as a
// push. A caller's context can cut it shorter: under the 20-second budget of
// phase 3, each send takes the smaller of the two.
const ChannelSendTimeout = 10 * time.Second

// newRouter builds a shoutrrr router and discards the library's own logger.
// Its lines routinely carry the URL that it was given, and a channel URL is
// a secret that must never reach the log (§10.4).
func newRouter(opts types.SenderOptions) *router.ServiceRouter {
	r, _ := router.NewWithOptions(util.DiscardLogger, opts) // no URLs, so no error
	return r
}

// ValidateChannelURL is the parse-before-write of `nag channel add` (§7.4).
// The URL goes through the shoutrrr router exactly as a send builds it. So
// the prompt refuses a typo, and the typo is not stored and found weeks
// later as a delivery_error.
//
// The router of shoutrrr v0.21.2 panics on some malformed custom URLs. For
// example, on a generic URL whose query does not parse, it formats the nil
// URL that it got back with the error. This function turns the panic into
// the refusal that the router failed to return. It does not keep the panic
// value, because that value can hold a part of the URL.
func ValidateChannelURL(rawURL string) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("the URL could not be parsed")
		}
	}()
	_, err = newRouter(types.SenderOptions{}).Locate(rawURL)
	return err
}

// SendChannel sends one message to one channel URL. It returns "" on
// success, or the §4.1 classification of the failure: timeout, refused,
// dns, http <code>, or send failed. It never returns the library's text,
// because that text routinely embeds the URL.
//
// This function applies the bound, because the shoutrrr API cannot carry it
// (§7.2). The send runs on its own goroutine and writes into a buffered
// channel of one. The caller selects on that channel and on a context with
// the 10-second timeout. That context derives from ctx, so a shorter caller
// deadline wins. If the context expires first, it abandons the send and the
// verdict is timeout. That word means "attempted, did not complete",
// whether a deadline or a shutdown ended the send.
//
// §7.2 expects the abandoned goroutine to end on the timeout of its own
// client. Most v0.21.2 services bound themselves, but not all of them.
// pushbullet sends with context.Background() on a bare http.Client. Without
// the timed client that sendOnce injects, generic does the same. So this
// function bounds the leak, whatever the service does. While an earlier send
// to the same URL is still running, and has run for less than busyFor, no
// new send starts and the verdict is NotAttempted. Nothing was attempted, so
// nothing is recorded for it (§4.1). The sweep logs the skip. A host that
// hangs forever then costs one goroutine per busyFor, not one per reminder.
// After busyFor, the channel is tried again and is not muted for good.
//
// A service with a context-aware send also gets the context, so the
// deadline and a shutdown reach the socket. In v0.21.2, smtp and xmpp get it
// through types.ContextSender. matrix, zulip and pagerduty get it through
// their own SendWithContext.
func SendChannel(ctx context.Context, rawURL, message string) string {
	slot := &sendSlot{started: time.Now()}
	if prev, busy := inFlight.LoadOrStore(rawURL, slot); busy {
		if time.Since(prev.(*sendSlot).started) < busyFor {
			return NotAttempted
		}
		// stuck past any timeout that a service can have: take over the slot
		inFlight.Store(rawURL, slot)
	}
	sendCtx, cancel := context.WithTimeout(ctx, ChannelSendTimeout)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		err := sendOnce(sendCtx, rawURL, message)
		// release the slot before the verdict goes back, so a caller that
		// sends again at once never finds its own finished send still busy.
		// Release only the slot of this send, never one that took it over.
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

// busyFor is how long a running send keeps new sends to its URL from
// starting: about one sweep tick. It is short on purpose. Inside this
// window, a channel is skipped silently: its rows record nothing and are
// never sent again (§7.2: fan-out must never block). So a long window hides
// every failure but the first. After the window, a new send starts beside
// the old one. It carries a different reminder, never the same message
// twice. It costs one extra goroutine per window while the old send runs
// on. Some sends do run on: discord retries on its own clock for five
// minutes and more. So a hung channel holds at most one goroutine per
// busyFor for that long. A pushbullet send never ends, so a hung pushbullet
// channel holds one goroutine per busyFor until the host closes.
var busyFor = 3 * ChannelSendTimeout

type sendSlot struct{ started time.Time }

// inFlight maps a channel URL to the send currently running for it,
// abandoned or not.
var inFlight sync.Map

// timedClientServices get an injected http.Client that carries the send
// timeout. So an abandoned send ends on its own and frees its inFlight
// slot, and does not mute the channel until the host closes the socket.
// The set holds only generic: generic falls back to a bare &http.Client{}
// with context.Background(), and its SetHTTPClient only swaps the field.
// The router injects after a service has initialized. For other services,
// this replaces a client that the service configured itself. ntfy and
// gotify lose their TLS options (disabletlsverification,
// insecureskipverify). pushbullet loses the Access-Token header that it set
// on its own client. So an injection into any other service breaks the
// sends that it was meant to bound. When the pin moves, re-check this set.
var timedClientServices = map[string]bool{"generic": true}

// abandonedSendTimeout is the timeout of the injected client. It is a
// variable so that a test can shorten it.
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
	switch s := service.(type) {
	case types.ContextSender:
		return s.SendContext(ctx, message, nil)
	case sendWithContext:
		return s.SendWithContext(ctx, message, nil)
	}
	return service.Send(message, nil)
}

// sendWithContext is the context-aware send that matrix, zulip and
// pagerduty export under their own name, outside types.ContextSender.
type sendWithContext interface {
	SendWithContext(ctx context.Context, message string, params *types.Params) error
}

// classifyChannelError reads the error's type and never its text (§4.1). A
// status appears only when the library surfaces it as a value. A code that
// exists only inside a sentence is not a value. Anything that cannot be
// typed is "send failed": a floor, not a fallback. Only a failure status
// counts. jsonclient also returns its Error for a 2xx whose body is not
// JSON (the login page of an auth proxy). In a column of failures, "http
// 200" reads as a contradiction.
func classifyChannelError(err error) string {
	if code := failureStatus(err); code != 0 {
		return fmt.Sprintf("http %d", code)
	}
	return classifySendError(err)
}

func failureStatus(err error) int {
	// jsonclient returns its Error by value: it has a value receiver
	var status jsonclient.Error
	if errors.As(err, &status) && status.StatusCode >= 400 {
		return status.StatusCode
	}
	return 0
}
