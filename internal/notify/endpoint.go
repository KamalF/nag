package notify

import (
	"net/url"
	"unicode/utf8"
)

// TruncateEndpoint renders a push endpoint as scheme, host, and the last 8
// characters of the path (§7.5): enough to tell two browsers apart, never
// enough to push to one. The full URL is the capability to notify that
// device and is never written to a log or a response in full (§7.1, §10.4).
func TruncateEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "unparseable-endpoint"
	}
	path := u.Path
	if len(path) > 8 {
		// The cut moves forward to a rune boundary, the same care
		// truncateBytes takes (§7.1): url.Parse percent-decodes into Path, so
		// a non-ASCII endpoint would otherwise be halved mid-rune and reach
		// §7.5's response and the §10.4 log line as a replacement character.
		cut := len(path) - 8
		for cut < len(path) && !utf8.RuneStart(path[cut]) {
			cut++
		}
		// the leading slash is kept: §7.5's printed example reads
		// https://…mozilla.com/…f3Ka9tQ2, and this string is quoted
		// verbatim in the push-test response
		path = "/…" + path[cut:]
	}
	return u.Scheme + "://" + u.Host + path
}
