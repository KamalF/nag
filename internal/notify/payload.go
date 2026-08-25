package notify

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/KamalF/nag/internal/store"
)

// §7.1's two payload caps. texts carries at most three entries whatever n
// says — the worker derives "+11 more" from n - texts.length, so there is
// no fourth field — and in the multi-reminder form each entry is cut to
// 120 bytes. Together they are what keeps a 60-row backlog inside the ~4 KB
// encrypted-payload ceiling: text alone is allowed 1000 bytes (§8.3), so
// one entry per reminder would exceed it on a backlog of five.
const (
	maxTexts     = 3
	maxTextBytes = 120
)

// pushPayload is the §7.1 wire form, and the two pointers are the whole
// reason it is a struct: due_at is present only when n == 1 — with several
// reminders there is no single moment to time — and id only when n == 1
// and there is a real row to point at, which the test push (§7.5) has not.
// So a consumer must check for the field rather than infer it from n, and
// omitempty on a pointer is what makes "absent" expressible.
type pushPayload struct {
	N     int      `json:"n"`
	ID    *int64   `json:"id,omitempty"`
	DueAt *int64   `json:"due_at,omitempty"`
	Texts []string `json:"texts"`
}

// buildDigestPayload renders the one message phase 2 sends to every
// subscription (§7.1). held is in due order and never empty; the rows
// beyond the third contribute to n only.
func buildDigestPayload(held []store.HeldRow) ([]byte, error) {
	p := pushPayload{N: len(held)}
	if len(held) == 1 {
		// the single form carries the full text, untruncated
		p.ID, p.DueAt = &held[0].ID, &held[0].DueAt
		p.Texts = []string{flattenLinks(held[0].Text)}
		return json.Marshal(p)
	}
	for _, r := range held[:min(len(held), maxTexts)] {
		p.Texts = append(p.Texts, truncateBytes(flattenLinks(r.Text), maxTextBytes))
	}
	return json.Marshal(p)
}

// truncateBytes cuts s to at most limit bytes on a rune boundary and marks
// the cut with an ellipsis. It counts bytes rather than runes because the
// payload ceiling is a byte budget, and it walks back to a boundary
// because a half-written rune renders as a replacement character on the
// device.
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// testPushText is §7.5's fixed text, pinned so the worker draws the test
// notification with no special case.
const testPushText = "Nag test notification"

// TestPayload is §7.5's payload: the n == 1 form with no id — no row
// exists to point at, which is why §7.1 states id separately from n == 1 —
// and due_at set to the server's now, so the body reads through the
// ordinary formatter (§9.11) instead of needing a second shape in sw.js.
func TestPayload(now int64) ([]byte, error) {
	return json.Marshal(pushPayload{N: 1, DueAt: &now, Texts: []string{testPushText}})
}
