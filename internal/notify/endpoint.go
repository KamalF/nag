package notify

import (
	"net/url"
	"unicode/utf8"
)

// TruncateEndpoint renders a push endpoint as scheme, host, and the last 8
// characters of the path (§7.5). That is enough to tell two browsers apart,
// and never enough to push to one. The full URL is the capability to notify
// that device. It is never written in full to a log or a response (§7.1,
// §10.4).
func TruncateEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "unparseable-endpoint"
	}
	path := u.Path
	if len(path) > 8 {
		// The cut moves forward to a rune boundary. truncateBytes also cuts
		// on a rune boundary (§7.1). url.Parse percent-decodes into Path. Without this step, the
		// cut halves a non-ASCII endpoint mid-rune, and the §7.5 response and
		// the §10.4 log line show a replacement character.
		cut := len(path) - 8
		for cut < len(path) && !utf8.RuneStart(path[cut]) {
			cut++
		}
		// keep the leading slash: the printed example of §7.5 reads
		// https://…mozilla.com/…f3Ka9tQ2, and the push-test response quotes
		// this string verbatim
		path = "/…" + path[cut:]
	}
	return u.Scheme + "://" + u.Host + path
}
