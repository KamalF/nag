package notify

import (
	"github.com/nicholas-fedor/shoutrrr/pkg/router"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"
	"github.com/nicholas-fedor/shoutrrr/pkg/util"
)

// newRouter builds a shoutrrr router for one channel URL. The library's own
// logger is discarded: its lines routinely carry the URL it was given, and a
// channel URL is a secret that must never reach the log (§10.4).
func newRouter(rawURL string, opts types.SenderOptions) (*router.ServiceRouter, error) {
	return router.NewWithOptions(util.DiscardLogger, opts, rawURL)
}

// ValidateChannelURL is `nag channel add`'s parse-before-write (§7.4): the
// URL goes through shoutrrr's router exactly as a send would build it, so a
// typo is refused at the prompt rather than stored and found weeks later
// as a delivery_error.
func ValidateChannelURL(rawURL string) error {
	_, err := newRouter(rawURL, types.SenderOptions{})
	return err
}
