package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
// "attempted, did not complete", whether a deadline or a shutdown ended it —
// and the goroutine finishes on its own client's timeout and is discarded.
// Where a service offers SendContext it gets the context, and every
// service that takes one gets an http.Client with the same timeout; but the
// bound never depends on either.
func SendChannel(ctx context.Context, rawURL, message string) string {
	sendCtx, cancel := context.WithTimeout(ctx, ChannelSendTimeout)
	defer cancel()

	result := make(chan error, 1)
	go func() { result <- sendOnce(sendCtx, rawURL, message) }()
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

func sendOnce(ctx context.Context, rawURL, message string) error {
	r := newRouter(types.SenderOptions{
		HTTPClient: &http.Client{Timeout: ChannelSendTimeout},
		Timeout:    ChannelSendTimeout,
	})
	service, err := r.Locate(rawURL)
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
// typed is "send failed": a floor, not a fallback.
func classifyChannelError(err error) string {
	var status jsonclient.Error
	if errors.As(err, &status) && status.StatusCode > 0 {
		return fmt.Sprintf("http %d", status.StatusCode)
	}
	var statusPtr *jsonclient.Error
	if errors.As(err, &statusPtr) && statusPtr.StatusCode > 0 {
		return fmt.Sprintf("http %d", statusPtr.StatusCode)
	}
	return classifySendError(err)
}
