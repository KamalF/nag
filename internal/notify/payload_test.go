package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/KamalF/nag/internal/store"
)

func held(n int, text string) []store.HeldRow {
	rows := make([]store.HeldRow, n)
	for i := range rows {
		rows[i] = store.HeldRow{ID: int64(i + 1), Text: text, DueAt: t0 + int64(i)}
	}
	return rows
}

// The two §7.1 forms, verified on the wire rather than on the struct: the
// absence of id and due_at is the part a consumer reads.
func TestPayloadForms(t *testing.T) {
	single, err := buildDigestPayload([]store.HeldRow{{ID: 123, Text: "Renew the domain", DueAt: t0}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"n":1,"id":123,"due_at":1786742400,"texts":["Renew the domain"]}`
	if string(single) != want {
		t.Errorf("single form:\n got %s\nwant %s", single, want)
	}

	multi, err := buildDigestPayload(held(14, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(multi), "id") || strings.Contains(string(multi), "due_at") {
		t.Errorf("multi form carries id or due_at: %s", multi)
	}
}

// texts is capped at three whatever n says — the worker derives "+11 more"
// from n - texts.length (§9.5), so a fourth entry would be payload spent
// on something nothing reads.
func TestPayloadTextsCappedAtThree(t *testing.T) {
	raw, err := buildDigestPayload(held(14, "x"))
	if err != nil {
		t.Fatal(err)
	}
	var p pushPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.N != 14 || len(p.Texts) != 3 {
		t.Fatalf("want n=14 with 3 texts, got n=%d with %d", p.N, len(p.Texts))
	}
}

// The single form carries the full text; only the multi form truncates.
// A 1000-byte reminder (§8.3) is most of the single form's payload, which
// is still an order of magnitude inside the ~4 KB ceiling.
func TestPayloadTruncatesOnlyTheMultiForm(t *testing.T) {
	long := strings.Repeat("a", 1000)

	single, err := buildDigestPayload([]store.HeldRow{{ID: 1, Text: long, DueAt: t0}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(single), long) {
		t.Error("single form truncated the full text")
	}

	multi, err := buildDigestPayload(held(2, long))
	if err != nil {
		t.Fatal(err)
	}
	var p pushPayload
	if err := json.Unmarshal(multi, &p); err != nil {
		t.Fatal(err)
	}
	for _, text := range p.Texts {
		if !strings.HasSuffix(text, "…") || len(text) > maxTextBytes+len("…") {
			t.Errorf("entry not cut to %d bytes with an ellipsis: %d bytes, %q", maxTextBytes, len(text), text)
		}
	}
}

// The cut lands on a rune boundary: a half-written rune renders as a
// replacement character on the device.
func TestPayloadTruncatesOnARuneBoundary(t *testing.T) {
	// "é" is two bytes, so a byte-count cut at 120 lands mid-rune
	got := truncateBytes(strings.Repeat("é", 100), maxTextBytes)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got)
	}
	if len(got) > maxTextBytes+len("…") {
		t.Errorf("truncation overran the budget: %d bytes", len(got))
	}
}

// §9.10: the notification shows the label, not the markdown. Both forms go
// through flattenLinks — this is the only place the server touches text.
func TestPayloadFlattensLinks(t *testing.T) {
	raw, err := buildDigestPayload([]store.HeldRow{{ID: 1, Text: "Renew [the domain](https://example.com)", DueAt: t0}})
	if err != nil {
		t.Fatal(err)
	}
	var p pushPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Texts[0] != "Renew the domain" {
		t.Errorf("texts[0] = %q, want the flattened form", p.Texts[0])
	}
}
