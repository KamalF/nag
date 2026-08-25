package notify

import "net/url"

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
		// the leading slash is kept: §7.5's printed example reads
		// https://…mozilla.com/…f3Ka9tQ2, and this string is quoted
		// verbatim in the push-test response
		path = "/…" + path[len(path)-8:]
	}
	return u.Scheme + "://" + u.Host + path
}
