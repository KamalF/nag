package notify

import (
	"regexp"
	"strings"
)

// linkPattern is the whole grammar of §9.10: [label](target), with a
// non-empty label that has no brackets, and a target with no whitespace and
// no ")". The client renderer carries the same regex. Both sides must agree
// on every row of the §9.10 case table.
var linkPattern = regexp.MustCompile(`\[([^\[\]]+)\]\(([^\s)]*)\)`)

// flattenLinks replaces each match with its label only when the client
// renders that match as a link: scheme http or https, and no "(" in the
// target. Anything else stays exactly as written, brackets and all. Both
// halves of the gate are load-bearing (§9.10). Without the gate, a flatten
// turns [a](mailto:x@y.z) into "a" while the row renders it literally. It
// also turns [a](javascript:alert(1)) into "a)". Push payloads (§7.1) and
// shoutrrr messages (§7.2) go through this function. It is the only place
// where the server touches text.
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
// renderer. It decides link-vs-literal, never validity. A refused target is
// emitted literally, not rejected (§8.3 has no server-side rule here).
func rendersAsLink(target string) bool {
	if strings.Contains(target, "(") {
		// the grammar cut a paren-bearing URL at its first ")", and a
		// refusal keeps that failure visible (§9.10)
		return false
	}
	// Read the scheme off the prefix, case-insensitively, and check nothing
	// else. A full parse adds a third condition that §9.10 does not have.
	// Also, Go's strict url.Parse refuses targets like ".../50%" that the
	// lenient parser of the browser accepts. The two sides then disagree on
	// a row that the client renders as a link.
	lower := strings.ToLower(target)
	return strings.HasPrefix(lower, "http:") || strings.HasPrefix(lower, "https:")
}
