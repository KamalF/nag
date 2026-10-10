package notify

import (
	"github.com/nicholas-fedor/shoutrrr/pkg/services/chat/matrix"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/chat/xmpp"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/chat/zulip"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/email/smtp"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/incident/pagerduty"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"
)

// sendOnce hands the context only to services that offer a context-aware
// send; one that stops matching its interface after a shoutrrr bump would
// fall back to Send on context.Background() without a test noticing. These
// assertions make that a compile error instead.
var (
	_ types.ContextSender = (*smtp.Service)(nil)
	_ types.ContextSender = (*xmpp.Service)(nil)
	_ sendWithContext     = (*matrix.Service)(nil)
	_ sendWithContext     = (*zulip.Service)(nil)
	_ sendWithContext     = (*pagerduty.Service)(nil)
)
