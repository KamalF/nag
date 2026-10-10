package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KamalF/nag/internal/store"
)

// The §7.6 table is normative and this file is it, read row by row against
// a fake clock and a fake push service. t0 is the spec's own example
// timestamp; every test states its times as offsets from it in seconds.
const t0 = 1786742400

// fakeSender records what phase 2 sent, one entry per send — the count is
// how the per-subscription tests read "to whom". The sweep ignores the
// verdict, so one canned result covers every test that needs a failure.
type fakeSender struct {
	payloads []string
	result   SendResult
}

func (f *fakeSender) SendTo(_ context.Context, _ time.Duration, payload []byte, _ store.Subscription) SendResult {
	f.payloads = append(f.payloads, string(payload))
	return f.result
}

type fixture struct {
	t      *testing.T
	st     *store.Store
	sweep  *Sweep
	sender *fakeSender
	logs   *syncBuffer
	clock  int64
}

// newFixture drives tick directly rather than Run: the cooldown is a
// clock comparison, and a real ticker would make every window test a
// 30-minute one.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "nag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{t: t, st: st, sender: &fakeSender{}, logs: &syncBuffer{}}
	f.sweep = NewSweep(st, slog.New(slog.NewTextHandler(f.logs, nil)), f.sender)
	f.sweep.now = func() time.Time { return time.Unix(f.clock, 0) }
	return f
}

// tickAt runs one sweep pass with the tick's single `now` set to t0+offset.
func (f *fixture) tickAt(offset int64) {
	f.t.Helper()
	f.clock = t0 + offset
	f.sweep.tick(f.t.Context())
}

// create writes a reminder due at t0+offset, as if written well before it.
func (f *fixture) create(text string, offset int64) store.Reminder {
	f.t.Helper()
	r, err := f.st.CreateReminder(f.t.Context(), text, t0+offset, nil, t0-3600)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) subscribe(endpoint string) {
	f.t.Helper()
	if err := f.st.UpsertSubscription(f.t.Context(), endpoint, "p256dh", "auth", "key", t0-3600); err != nil {
		f.t.Fatal(err)
	}
}

// sent decodes the payloads phase 2 handed the sender, one per send.
func (f *fixture) sent() []pushPayload {
	f.t.Helper()
	out := make([]pushPayload, len(f.sender.payloads))
	for i, raw := range f.sender.payloads {
		if err := json.Unmarshal([]byte(raw), &out[i]); err != nil {
			f.t.Fatalf("payload %d is not json: %v", i, err)
		}
	}
	return out
}

// onlyPush asserts exactly one push went out and returns it.
func (f *fixture) onlyPush() pushPayload {
	f.t.Helper()
	sent := f.sent()
	if len(sent) != 1 {
		f.t.Fatalf("want exactly 1 push, got %d: %v", len(sent), f.sender.payloads)
	}
	return sent[0]
}

func (f *fixture) noPush() {
	f.t.Helper()
	if len(f.sender.payloads) != 0 {
		f.t.Fatalf("want no push, got %v", f.sender.payloads)
	}
}

// "One reminder comes due, device online — 1, at the due time": the n == 1
// form, so the worker can draw the text as the title and click through to
// the row.
func TestDueRowSendsSingleForm(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	r := f.create("Renew the domain", 0)

	f.tickAt(10)

	got := f.onlyPush()
	if got.N != 1 || got.ID == nil || *got.ID != r.ID || got.DueAt == nil || *got.DueAt != t0 {
		t.Fatalf("want n=1 with id=%d due_at=%d, got %+v", r.ID, t0, got)
	}
	if len(got.Texts) != 1 || got.Texts[0] != "Renew the domain" {
		t.Errorf("texts = %v", got.Texts)
	}
}

// "Two reminders due in the same tick — 1, one digest, n == 2". id and
// due_at must be absent: with several there is no single moment to time
// and no single row to point at.
func TestTwoRowsInOneTickAreOneDigest(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("first", -20)
	f.create("second", -10)

	f.tickAt(0)

	got := f.onlyPush()
	if got.N != 2 || got.ID != nil || got.DueAt != nil {
		t.Fatalf("want n=2 with no id and no due_at, got %+v", got)
	}
	if want := []string{"first", "second"}; !slices.Equal(got.Texts, want) {
		t.Errorf("texts = %v, want %v (due order)", got.Texts, want)
	}
}

// "A reminder comes due 7 minutes after the last notification — 1, but at
// the 30-minute mark". The held tick logs what it is holding and how long
// is left, which is what distinguishes the silence from a broken sweep.
func TestRowDueInsideTheWindowWaitsForIt(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("first", 0)
	f.tickAt(0)

	f.create("held", 420) // 7 minutes later
	f.tickAt(430)

	if n := len(f.sender.payloads); n != 1 {
		t.Fatalf("second row was pushed inside the window: %d pushes", n)
	}
	if !strings.Contains(f.logs.String(), "held=1") ||
		!strings.Contains(f.logs.String(), "seconds_remaining=1370") {
		t.Errorf("held line missing or wrong: %q", f.logs.String())
	}

	f.tickAt(1800)

	sent := f.sent()
	if len(sent) != 2 || sent[1].N != 1 {
		t.Fatalf("want a second push with n=1 when the window opens, got %+v", sent)
	}
}

// The same row's second half: "Had the stretch started with an open
// window, the first would have pinged on time and the other five would
// have arrived as a second digest with n == 5."
func TestRowsHeldAcrossTheWindowRideOneDigest(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("opener", 0)
	f.tickAt(0)

	for i := range 5 {
		f.create(fmt.Sprintf("held %d", i), int64(300*(i+1)))
		f.tickAt(int64(300*(i+1) + 5))
	}
	f.tickAt(1800)

	sent := f.sent()
	if len(sent) != 2 || sent[1].N != 5 {
		t.Fatalf("want one opener push then one n=5 digest, got %+v", sent)
	}
	if len(sent[1].Texts) != maxTexts {
		t.Errorf("texts = %v, want %d entries whatever n says", sent[1].Texts, maxTexts)
	}
}

// "Server down 2 days, 40 reminders due — 0": every row is past the gate at
// mark time, so it is stamped silently with one INFO line and phase 2 finds
// nothing. The window is untouched, which the second tick proves.
func TestRowPastTheGateIsSilentAndKeepsTheWindow(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	late := f.create("31 minutes late", 0)

	f.tickAt(1860)

	f.noPush()
	if !markedTooLate(f.logs.String(), late.ID) {
		t.Errorf("no too-late line for id=%d: %q", late.ID, f.logs.String())
	}

	f.create("on time", 1870)
	f.tickAt(1880)

	if got := f.onlyPush(); got.N != 1 {
		t.Fatalf("the gate consumed the window: %+v", got)
	}
}

// "Server down 25 minutes, 60 reminders due and all inside the gate — 1
// with n == 50, then 1 with n == 10 half an hour later." The only case
// where phase 1's LIMIT 50 and the cooldown compound.
func TestSixtyRowBacklogSplitsAcrossTwoDigests(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	for i := range 60 {
		f.create(fmt.Sprintf("backlog %d", i), int64(i))
	}

	f.tickAt(100)
	f.tickAt(130) // marks the last ten; the window has just closed
	f.tickAt(1900)

	sent := f.sent()
	if len(sent) != 2 || sent[0].N != 50 || sent[1].N != 10 {
		t.Fatalf("want n=50 then n=10, got %+v", sent)
	}
}

// "A held reminder is cleared before the window opens — 0 for that row."
// Phase 2 filters on done_at IS NULL; you already dealt with it.
func TestHeldRowClearedDropsOutOfTheDigest(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("opener", 0)
	f.tickAt(0)
	held := f.create("cleared while held", 60)
	f.tickAt(70)

	if _, err := f.st.MarkDone(t.Context(), held.ID, t0+100); err != nil {
		t.Fatal(err)
	}
	f.tickAt(1800)

	if n := len(f.sender.payloads); n != 1 {
		t.Fatalf("cleared row still rode a digest: %d pushes", n)
	}
}

// "The same held row, undone an hour later — 0, until you re-time it."
// Undone stamps pushed_at on a row whose notified_at is set, so it is back
// in the list silently.
func TestHeldRowClearedThenUndoneStaysSilent(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("opener", 0)
	f.tickAt(0)
	held := f.create("cleared while held", 60)
	f.tickAt(70)

	if _, err := f.st.MarkDone(t.Context(), held.ID, t0+100); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.MarkUndone(t.Context(), held.ID, t0+3600); err != nil {
		t.Fatal(err)
	}
	f.tickAt(3700)

	if n := len(f.sender.payloads); n != 1 {
		t.Fatalf("undone held row re-entered the digest: %d pushes", n)
	}
}

// "A reminder is cleared while still in Waiting quietly, then undone 6
// minutes after its due time — 1, within one tick of the undo." This row
// genuinely never fired: a done row is invisible to phase 1, so both
// columns are still NULL and the undo hands phase 1 a live candidate.
func TestRowClearedBeforeItFiredNotifiesAfterTheUndo(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	r := f.create("cleared before it fired", 600)

	if _, err := f.st.MarkDone(t.Context(), r.ID, t0); err != nil {
		t.Fatal(err)
	}
	f.tickAt(700) // due and done: phase 1 cannot see it
	f.noPush()

	if _, err := f.st.MarkUndone(t.Context(), r.ID, t0+960); err != nil {
		t.Fatal(err)
	}
	f.tickAt(970) // 6 minutes late, inside the gate

	if got := f.onlyPush(); got.N != 1 || got.ID == nil || *got.ID != r.ID {
		t.Fatalf("want a push for id=%d, got %+v", r.ID, got)
	}
}

// "The same row, undone 40 minutes after its due time — 0": now past the
// too-late gate, marked silently with one INFO line.
func TestRowUndonePastTheGateIsSilent(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	r := f.create("cleared before it fired", 0)

	if _, err := f.st.MarkDone(t.Context(), r.ID, t0-10); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.MarkUndone(t.Context(), r.ID, t0+2400); err != nil {
		t.Fatal(err)
	}
	f.tickAt(2410)

	f.noPush()
	if !markedTooLate(f.logs.String(), r.ID) {
		t.Errorf("no too-late line: %q", f.logs.String())
	}
}

// "A reminder is re-timed while held — 0 now, 1 at the new time." Re-time
// nulls both columns, so the row leaves the held set and re-enters phase 1.
func TestHeldRowRetimedLeavesAndComesBack(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("opener", 0)
	f.tickAt(0)
	held := f.create("re-timed while held", 60)
	f.tickAt(70)

	newDue := int64(t0 + 5000)
	if _, err := f.st.UpdateReminder(t.Context(), held, store.ReminderUpdate{DueAt: &newDue}, t0+100); err != nil {
		t.Fatal(err)
	}
	f.tickAt(1800)
	if n := len(f.sender.payloads); n != 1 {
		t.Fatalf("re-timed row rode the digest it had left: %d pushes", n)
	}

	f.tickAt(5010)
	sent := f.sent()
	if len(sent) != 2 || sent[1].ID == nil || *sent[1].ID != held.ID {
		t.Fatalf("want a push for the re-timed row at its new time, got %+v", sent)
	}
}

// "Every send in a digest fails — 0, plus a log line per endpoint": the
// rows are gone from the held set and the window is consumed too, so the
// failure is not retried on the next tick.
func TestFailedSendsStillConsumeTheWindow(t *testing.T) {
	f := newFixture(t)
	f.sender.result = SendResult{Err: "timeout"}
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("first", 0)
	f.tickAt(0)

	f.create("second", 60)
	f.tickAt(70)

	if n := len(f.sender.payloads); n != 1 {
		t.Fatalf("a failed digest reopened the window: %d pushes", n)
	}
}

// "Zero subscriptions stored — 0, plus a WARNING naming the count". The
// rows are still marked and the window is still consumed, because phase 2
// did select rows.
func TestZeroSubscriptionsWarnsAndConsumesTheWindow(t *testing.T) {
	f := newFixture(t)
	first := f.create("first", 0)
	f.create("second", 10)
	f.tickAt(20)

	logs := f.logs.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "count=2") ||
		!strings.Contains(logs, fmt.Sprintf("first_id=%d", first.ID)) {
		t.Fatalf("no zero-subscription warning naming the count and first id: %q", logs)
	}

	f.subscribe("https://push.example/aaaaaaaa")
	f.create("third", 100)
	f.tickAt(110)
	f.noPush()
}

// "Three devices subscribed — 1 each": one call per subscription with the
// same payload.
func TestEverySubscriptionGetsTheSamePayload(t *testing.T) {
	f := newFixture(t)
	for _, e := range []string{"https://a.example/aaaaaaaa", "https://b.example/bbbbbbbb", "https://c.example/cccccccc"} {
		f.subscribe(e)
	}
	f.create("Renew the domain", 0)

	f.tickAt(10)

	if len(f.sender.payloads) != 3 {
		t.Fatalf("want one send per subscription, got %d", len(f.sender.payloads))
	}
	for i, p := range f.sender.payloads {
		if p != f.sender.payloads[0] {
			t.Errorf("send %d carried a different payload: %q vs %q", i, p, f.sender.payloads[0])
		}
	}
}

// §10.4: the push line is the only record of which reminders a
// notification was about, and it names ids, never texts.
func TestPushLineNamesIDsAndNeverText(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	a := f.create("wash the dog", 0)
	b := f.create("book the flights", 10)

	f.tickAt(20)

	logs := f.logs.String()
	if !strings.Contains(logs, "push sent") || !strings.Contains(logs, "n=2") ||
		!strings.Contains(logs, fmt.Sprintf("ids=\"[%d %d]\"", a.ID, b.ID)) ||
		!strings.Contains(logs, "subscriptions=1") {
		t.Fatalf("push line missing n, ids or subscription count: %q", logs)
	}
	if strings.Contains(logs, "wash the dog") || strings.Contains(logs, "book the flights") {
		t.Errorf("sweep logged reminder text: %q", logs)
	}
}

// markedTooLate looks for the §7.3 gate's INFO line naming id.
func markedTooLate(logs string, id int64) bool {
	return strings.Contains(logs, "marked too late, no notification") &&
		strings.Contains(logs, fmt.Sprintf("id=%d", id))
}

// "Server down 40 minutes, 3 due at the start and 2 near the end — 1,
// n == 2." The only row where phase 1 gates part of a tick and passes the
// rest in the same transaction, so it is the one that pins the split
// reaching phase 2 rather than the whole marked set.
func TestMixedTickDigestsOnlyTheRowsInsideTheGate(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	var gated []int64
	for i := range 3 {
		gated = append(gated, f.create(fmt.Sprintf("past the gate %d", i), int64(i)).ID)
	}
	f.create("inside a", 2300)
	f.create("inside b", 2350)

	f.tickAt(2400) // the first three are 2400 s late, the last two 100 and 50

	got := f.onlyPush()
	if got.N != 2 {
		t.Fatalf("n = %d, want 2 — the gated rows rode the digest", got.N)
	}
	if want := []string{"inside a", "inside b"}; !slices.Equal(got.Texts, want) {
		t.Errorf("texts = %v, want %v", got.Texts, want)
	}
	for _, id := range gated {
		if !markedTooLate(f.logs.String(), id) {
			t.Errorf("no too-late line for id=%d", id)
		}
	}
}

// The row the bound exists for: a reminder marked on time, held by a closed
// window, and still held when the server comes back from a long outage. The
// phase-1 gate cannot see it — it was fresh when it was marked — so phase 2
// is the only place that can refuse it. It ends up exactly where the gate
// would have put it: stamped, in the list, and silent.
func TestHeldRowStrandedByAnOutageIsSilent(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("opener", 0)
	f.tickAt(0) // consumes the window
	stranded := f.create("held when the lights went out", 60)
	f.tickAt(70) // marked on time, then held

	// the server is down for two days; the boot tick finds the window open
	f.tickAt(2 * 86400)

	if n := len(f.sender.payloads); n != 1 {
		t.Fatalf("%d pushes, want only the opener — a two-day-old row was notified about", n)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "held too long, no notification") ||
		!strings.Contains(logs, fmt.Sprintf("id=%d", stranded.ID)) {
		t.Errorf("no held-too-long line for id=%d: %q", stranded.ID, logs)
	}
}

// And it costs nothing that follows: refusing to send consumes no window, so
// a reminder coming due right after the restart pings on time.
func TestAStrandedHeldRowLeavesTheWindowOpen(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("opener", 0)
	f.tickAt(0)
	f.create("held when the lights went out", 60)
	f.tickAt(70)

	f.tickAt(2 * 86400) // the stranded row is stamped, nothing is sent
	f.create("back", 2*86400+30)
	f.tickAt(2*86400 + 40) // a fresh row, on a window that was never consumed

	sent := f.sent()
	if len(sent) != 2 || sent[1].N != 1 || sent[1].Texts[0] != "back" {
		t.Fatalf("want the fresh row pushed on time, got %+v", sent)
	}
}

// The compound case the bound must not break, and the reason it is 3600
// rather than 1800: phase 1's LIMIT 50 leaves rows for a later tick, which
// marks them nearly 1800 s late — and the digest that carries them is behind
// by nearly a full window on top of that, because the tick before it
// consumed one. That is ~3450 s here; the true worst case is 3570 s, pinned
// separately below.
func TestRowsMarkedLateAndHeldAFullWindowStillArrive(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	for i := range 60 {
		f.create(fmt.Sprintf("backlog %d", i), int64(i))
	}

	f.tickAt(1700) // the server comes back: 50 marked, all inside the gate, digested
	f.tickAt(1730) // the last ten, ~1700 s late at mark, held by the window
	f.tickAt(3500) // the window reopens: they are now ~3445 s late

	sent := f.sent()
	if len(sent) != 2 || sent[0].N != 50 || sent[1].N != 10 {
		t.Fatalf("want n=50 then n=10, got %+v", sent)
	}
	if strings.Contains(f.logs.String(), "held too long") {
		t.Error("the bound refused rows that the two 30-minute constants allow")
	}
}

// The worst lateness the sweep can legitimately produce, which is exactly
// what the bound has to clear. Phase 1's LIMIT 50 leaves rows for the next
// tick, where they are marked at the gate's own edge — 1800 s late — and the
// window they then wait on was consumed by the tick before them, so it opens
// nearly a full 1800 s after that. 3570 s, and it must arrive.
func TestTheWorstLegitimateLatenessStillArrives(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	for i := range 60 {
		f.create(fmt.Sprintf("backlog %d", i), -1770)
	}

	f.tickAt(0)    // marks 50, all 1770 s late, and digests them
	f.tickAt(30)   // marks the last ten, now 1800 s late: the gate's edge
	f.tickAt(1800) // the window reopens and they are 3570 s late

	sent := f.sent()
	if len(sent) != 2 || sent[0].N != 50 || sent[1].N != 10 {
		t.Fatalf("want n=50 then n=10, got %+v", sent)
	}
	if strings.Contains(f.logs.String(), "held too long") {
		t.Error("the bound refused a row the two 30-minute constants allow")
	}
}

// lastPushAt is memory, so after a backward clock step (NTP, a snapshot
// restore) a plain difference would read the window as closed for the whole
// length of the jump. An unrecognisable window counts as open.
func TestBackwardClockStepOpensTheWindow(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	f.create("first", 0)
	f.tickAt(0)
	f.sender.payloads = nil

	f.create("after the step", -600)
	f.tickAt(-600 + 10) // the clock stepped ten minutes back

	if got := f.onlyPush(); got.N != 1 {
		t.Fatalf("want the row pushed after a backward step, got %+v", got)
	}
}
