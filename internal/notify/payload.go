package notify

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/KamalF/nag/internal/store"
)

// The two payload caps of §7.1. texts carries at most three entries,
// whatever n says. The worker derives "+11 more" from n - texts.length, so
// there is no fourth field. In the multi-reminder form, each entry is cut to
// 120 bytes. Together, the caps keep a 60-row backlog inside the ~4 KB
// encrypted-payload ceiling. text alone can be 1000 bytes (§8.3). So with
// one entry per reminder, a backlog of five exceeds the ceiling.
const (
	maxTexts     = 3
	maxTextBytes = 120
	ellipsis     = "…"
)

// pushPayload is the §7.1 wire form. The two pointers are the whole reason
// that it is a struct. due_at is present only when n == 1: with several
// reminders, there is no single moment to time. id is present only when
// n == 1 and there is a real row to point at, which the test push (§7.5)
// does not have. So a consumer must check for the field, not infer it from
// n. With omitempty on a pointer, the field can be absent from the JSON.
type pushPayload struct {
	N     int      `json:"n"`
	ID    *int64   `json:"id,omitempty"`
	DueAt *int64   `json:"due_at,omitempty"`
	Texts []string `json:"texts"`
}

// buildDigestPayload renders the one message that phase 2 sends to every
// subscription (§7.1). held is in due order and never empty. The rows
// beyond the third contribute to n only.
func buildDigestPayload(held []store.HeldRow) ([]byte, error) {
	p := pushPayload{N: len(held)}
	if len(held) == 1 {
		// the single form carries the full text, untruncated
		p.ID, p.DueAt = &held[0].ID, &held[0].DueAt
		p.Texts = []string{flattenLinks(held[0].Text)}
		return marshalPayload(p)
	}
	for _, r := range held[:min(len(held), maxTexts)] {
		p.Texts = append(p.Texts, truncateBytes(flattenLinks(r.Text), maxTextBytes))
	}
	return marshalPayload(p)
}

// marshalPayload is json.Marshal without HTML escaping. json.Marshal writes
// each <, > and & as a six-byte \u escape. So a 1000-byte text of them
// (§8.3 allows it) becomes 6000 bytes, past the ~4 KB encrypted-payload
// ceiling. At that size, the send fails before any socket opens, and the
// row, already stamped, is never pushed. The worker reads the payload with
// JSON.parse, never as markup, so the escaping has no use.
func marshalPayload(p pushPayload) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// truncateBytes cuts s on a rune boundary, so that the result, ellipsis
// included, is at most limit bytes. It counts bytes, not runes, because the
// payload ceiling is a byte budget. The marker is inside that budget, not
// added on top of it, because §7.1 caps the entry that goes on the wire. It
// walks back to a boundary because a half-written rune renders as a
// replacement character on the device.
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// testPushText is the fixed text of §7.5. It is pinned so that the worker
// draws the test notification with no special case.
const testPushText = "Nag test notification"

// TestPayload is the payload of §7.5: the n == 1 form with no id, and with
// due_at set to the server's now. No row exists to point at, which is why
// §7.1 states id separately from n == 1. With due_at set, the body shows
// that time through the ordinary formatter (§9.11), and sw.js needs no
// second shape.
func TestPayload(now int64) ([]byte, error) {
	return marshalPayload(pushPayload{N: 1, DueAt: &now, Texts: []string{testPushText}})
}
