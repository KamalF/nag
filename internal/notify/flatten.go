package notify

import (
	"net/url"
	"regexp"
	"strings"
)

// linkPattern is §9.10's whole grammar: [label](target), label non-empty
// with no brackets, target with no whitespace and no ")". The client
// renderer carries the same regex; both sides must agree on every row of
// the §9.10 case table.
var linkPattern = regexp.MustCompile(`\[([^\[\]]+)\]\(([^\s)]*)\)`)

// flattenLinks replaces each match with its label only when that match
// would render as a link client-side — scheme http or https, no "(" in the
// target. Anything else stays exactly as written, brackets and all. Both
// halves of the gate are load-bearing (§9.10): an ungated flatten would
// turn [a](mailto:x@y.z) into "a" while the row renders it literally, and
// [a](javascript:alert(1)) into "a)". Push payloads (§7.1) and shoutrrr
// messages (§7.2) go through here — the only place the server touches text.
func flattenLinks(text string) string {
	return linkPattern.ReplaceAllStringFunc(text, func(match string) string {
		sub := linkPattern.FindStringSubmatch(match)
		label, target := sub[1], sub[2]
		if !rendersAsLink(target) {
			return match
		}
		return label
	})
}

// rendersAsLink is the eligibility gate, shared in spirit with the client
// renderer: it decides link-vs-literal, never validity — a refused target
// is emitted literally, not rejected (§8.3 has no server-side rule here).
func rendersAsLink(target string) bool {
	if strings.Contains(target, "(") {
		// a paren-bearing URL would have been truncated at the first ")"
		// by the grammar — refusing it keeps the failure visible (§9.10)
		return false
	}
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
