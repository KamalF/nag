package notify

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KamalF/nag/internal/store"
)

// The fan-out half of the §7.6 checklist, against the same fake
// clock as phase 2 and a fake channel sender standing in for the router.

// fakeChannels records each send and answers per URL: a verdict, or a
// block until the send's context ends (an unreachable host).
type fakeChannels struct {
	mu       sync.Mutex
	sent     []string // "url|message"
	verdicts map[string]string
	hang     map[string]bool
}

func (f *fakeChannels) send(ctx context.Context, url, message string) string {
	f.mu.Lock()
	f.sent = append(f.sent, url+"|"+message)
	hang := f.hang[url]
	verdict := f.verdicts[url]
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return "timeout"
	}
	return verdict
}

func (f *fakeChannels) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func withChannels(t *testing.T, f *fixture, names ...string) *fakeChannels {
	t.Helper()
	for _, n := range names {
		if err := f.st.AddChannel(t.Context(), n, "url://"+n); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeChannels{verdicts: map[string]string{}, hang: map[string]bool{}}
	f.sweep.channels = fc.send
	return fc
}

func (f *fixture) deliveryError(id int64) *string {
	f.t.Helper()
	r, err := f.st.GetReminder(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r.DeliveryError
}

// One message per reminder, link-flattened, to each enabled channel.
func TestFanOutSendsTheFlattenedText(t *testing.T) {
	f := newFixture(t)
	fc := withChannels(t, f, "ntfy", "tg")
	f.create("Renew [the domain](https://reg.example/x)", 0, "ntfy", "tg")

	f.tickAt(10)

	if want := []string{"url://ntfy|Renew the domain", "url://tg|Renew the domain"}; strings.Join(fc.sent, ",") != strings.Join(want, ",") {
		t.Errorf("sent %v, want %v", fc.sent, want)
	}
}

// "A row 31 minutes late at mark time is never fanned out either, not
// merely never pushed": the gate means no output at all.
func TestFanOutSkipsRowsPastTheGate(t *testing.T) {
	f := newFixture(t)
	fc := withChannels(t, f, "ntfy")
	f.create("late", -31*60, "ntfy")

	f.tickAt(0)

	if fc.count() != 0 {
		t.Errorf("a gate-stamped row was fanned out: %v", fc.sent)
	}
}

// "Fan-out fires in the same tick even when that tick's push is held."
func TestFanOutIsNotHeldByTheCooldown(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	fc := withChannels(t, f, "ntfy")
	f.create("first", 0)
	f.tickAt(0) // consumes the window
	f.sender.payloads = nil

	f.create("second", 7*60, "ntfy")
	f.tickAt(7*60 + 10)

	f.noPush()
	if fc.count() != 1 {
		t.Errorf("held row was not fanned out at once: %v", fc.sent)
	}
}

// Disabled channels are skipped silently. An orphan gets an INFO line.
// Neither is an attempt, so neither writes delivery_error.
func TestFanOutDisabledAndOrphan(t *testing.T) {
	f := newFixture(t)
	fc := withChannels(t, f, "muted")
	if err := f.st.SetChannelEnabled(t.Context(), "muted", false); err != nil {
		t.Fatal(err)
	}
	r := f.create("x", 0, "gone", "muted")

	f.tickAt(10)

	if fc.count() != 0 {
		t.Errorf("sent to a disabled or missing channel: %v", fc.sent)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "channel no longer exists") || !strings.Contains(logs, "channel=gone") {
		t.Errorf("no orphan line: %s", logs)
	}
	if strings.Contains(logs, "channel=muted") {
		t.Errorf("a disabled channel was logged: %s", logs)
	}
	if got := f.deliveryError(r.ID); got != nil {
		t.Errorf("delivery_error = %q with nothing attempted", *got)
	}
}

// The verdict is classified, capped at three entries, written per row, and
// cleared by a later fully successful fan-out.
func TestFanOutDeliveryError(t *testing.T) {
	f := newFixture(t)
	fc := withChannels(t, f, "a", "b", "c", "d", "e")
	for _, n := range []string{"a", "b", "c", "d"} {
		fc.verdicts["url://"+n] = "http 401"
	}
	r := f.create("x", 0, "a", "b", "c", "d", "e")

	f.tickAt(10)

	got := f.deliveryError(r.ID)
	want := "a: http 401; b: http 401; c: http 401; +1 more"
	if got == nil || *got != want {
		t.Fatalf("delivery_error = %v, want %q", got, want)
	}
	if !strings.Contains(f.logs.String(), want) {
		t.Errorf("failure not logged in classified form: %s", f.logs.String())
	}

	// a re-time clears it (§4.1), and the next fan-out, all successful,
	// leaves it clear
	fc.verdicts = map[string]string{}
	f.retime(r, 3600)
	f.tickAt(3600 + 10)
	if got := f.deliveryError(r.ID); got != nil {
		t.Errorf("delivery_error after a clean fan-out = %q", *got)
	}
}

func (f *fixture) retime(r store.Reminder, offset int64) {
	f.t.Helper()
	due := t0 + offset
	if _, err := f.st.UpdateReminder(f.t.Context(), r, store.ReminderUpdate{DueAt: &due}, t0); err != nil {
		f.t.Fatal(err)
	}
}

// "A phase 3 that exceeds its deadline drops the remaining rows, logs their
// ids, and does not delay the next tick's push."
func TestFanOutDeadlineDropsRemainingRows(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	fc := withChannels(t, f, "dead")
	fc.hang["url://dead"] = true
	f.sweep.budget = 150 * time.Millisecond
	first := f.create("one", 0, "dead")
	second := f.create("two", 1, "dead")
	third := f.create("three", 2, "dead")

	start := time.Now()
	f.tickAt(10)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("phase 3 ran %v past a 150 ms budget", took)
	}

	if fc.count() != 1 {
		t.Errorf("attempted %d sends, want 1 before the budget ran out", fc.count())
	}
	if got := f.deliveryError(first.ID); got == nil || *got != "dead: timeout" {
		t.Errorf("the cut-short send's verdict = %v, want \"dead: timeout\"", got)
	}
	for _, r := range []store.Reminder{second, third} {
		if got := f.deliveryError(r.ID); got != nil {
			t.Errorf("row %d was never attempted but carries %q", r.ID, *got)
		}
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "fan-out deadline reached") || !strings.Contains(logs, "count=2") {
		t.Errorf("dropped rows not logged: %s", logs)
	}

	// the next window's push is unaffected
	f.sender.payloads = nil
	f.create("later", 3600)
	f.tickAt(3600 + 10)
	if got := f.onlyPush(); got.N != 1 {
		t.Errorf("next push = %+v", got)
	}
}

// "A phase 3 whose deadline expires inside a row carrying many unreachable
// channels still returns at ~the budget": every send is bounded by the
// phase, not by its own full timeout.
func TestFanOutDeadlineBoundsARowOfDeadChannels(t *testing.T) {
	f := newFixture(t)
	names := []string{"c01", "c02", "c03", "c04", "c05", "c06", "c07", "c08",
		"c09", "c10", "c11", "c12", "c13", "c14", "c15", "c16"}
	fc := withChannels(t, f, names...)
	for _, n := range names {
		fc.hang["url://"+n] = true
	}
	f.sweep.budget = 150 * time.Millisecond
	r := f.create("x", 0, names...)

	start := time.Now()
	f.tickAt(10)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("a 16-dead-channel row ran %v past a 150 ms budget", took)
	}
	if fc.count() != 1 {
		t.Errorf("attempted %d channels after the budget expired, want 1", fc.count())
	}
	if got := f.deliveryError(r.ID); got == nil || *got != "c01: timeout" {
		t.Errorf("delivery_error = %v, want only the attempted channel", got)
	}
}

// "A tick whose phase 2 spent 30 seconds on unreachable subscriptions still
// attempts its fan-out": the budget is measured from the start of phase 3.
func TestFanOutBudgetStartsWithThePhase(t *testing.T) {
	f := newFixture(t)
	f.subscribe("https://push.example/aaaaaaaa")
	fc := withChannels(t, f, "ntfy")
	f.sweep.budget = 150 * time.Millisecond
	f.sweep.sender = slowPusher{delay: 300 * time.Millisecond}
	f.create("x", 0, "ntfy")

	f.tickAt(10)

	if fc.count() != 1 {
		t.Errorf("fan-out after a slow phase 2 attempted %d sends, want 1", fc.count())
	}
}

type slowPusher struct{ delay time.Duration }

func (p slowPusher) SendTo(context.Context, time.Duration, []byte, store.Subscription) SendResult {
	time.Sleep(p.delay)
	return SendResult{Err: "timeout"}
}

func TestFormatDeliveryError(t *testing.T) {
	for _, tt := range []struct {
		in   []string
		want string
	}{
		{[]string{"a: dns"}, "a: dns"},
		{[]string{"a: dns", "b: refused", "c: timeout"}, "a: dns; b: refused; c: timeout"},
		{[]string{"a: dns", "b: refused", "c: timeout", "d: http 500", "e: send failed"},
			"a: dns; b: refused; c: timeout; +2 more"},
	} {
		if got := formatDeliveryError(tt.in); got != tt.want {
			t.Errorf("format(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A shutdown mid-tick: fan-out stops at the row boundary and says why, and
// the purge is skipped rather than failing on the canceled context.
func TestShutdownMidFanOut(t *testing.T) {
	f := newFixture(t)
	fc := withChannels(t, f, "dead")
	fc.hang["url://dead"] = true
	f.create("one", 0, "dead")
	f.create("two", 1, "dead")

	ctx, cancel := context.WithCancel(t.Context())
	f.clock = t0 + 10
	go func() {
		for fc.count() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	f.sweep.tick(ctx)

	logs := f.logs.String()
	if !strings.Contains(logs, "fan-out stopped by shutdown") {
		t.Errorf("no shutdown reason: %s", logs)
	}
	if strings.Contains(logs, "sweep: purge") {
		t.Errorf("the purge ran on a canceled context: %s", logs)
	}
}

// A channel still busy with an earlier send is not attempted. Nothing is
// recorded for it (§4.1: the column describes attempts), and the skip is
// logged.
func TestFanOutSkipsABusyChannel(t *testing.T) {
	f := newFixture(t)
	fc := withChannels(t, f, "mail", "ntfy")
	fc.verdicts["url://mail"] = NotAttempted
	r := f.create("x", 0, "mail", "ntfy")

	f.tickAt(10)

	if got := f.deliveryError(r.ID); got != nil {
		t.Errorf("delivery_error = %q, want nothing recorded", *got)
	}
	if !strings.Contains(f.logs.String(), "still busy") {
		t.Errorf("busy skip not logged: %s", f.logs.String())
	}
}

// A channel-list edit while the send is in flight clears delivery_error
// (§4.1). The verdict computed before the edit names a channel that the row
// no longer carries, and it must not land on top.
func TestFanOutVerdictYieldsToAChannelEdit(t *testing.T) {
	f := newFixture(t)
	withChannels(t, f, "tg")
	r := f.create("x", 0, "tg")
	f.sweep.channels = func(ctx context.Context, url, message string) string {
		current, err := f.st.GetReminder(ctx, r.ID)
		if err != nil {
			t.Error(err)
		}
		none := []string{}
		if _, err := f.st.UpdateReminder(ctx, current, store.ReminderUpdate{ExtraChannels: &none}, t0); err != nil {
			t.Error(err)
		}
		return "timeout"
	}

	f.tickAt(10)

	if got := f.deliveryError(r.ID); got != nil {
		t.Errorf("delivery_error = %q after the channels were edited off", *got)
	}
}

// A re-time into the past, in the same second as phase 1, stamps
// notified_at with that same now. The verdict must still yield to it (§4.1:
// a re-time clears delivery_error).
func TestFanOutVerdictYieldsToASameSecondRetime(t *testing.T) {
	f := newFixture(t)
	withChannels(t, f, "tg")
	r := f.create("x", 0, "tg")
	f.sweep.channels = func(ctx context.Context, url, message string) string {
		current, err := f.st.GetReminder(ctx, r.ID)
		if err != nil {
			t.Error(err)
		}
		past := int64(t0 - 50)
		if _, err := f.st.UpdateReminder(ctx, current, store.ReminderUpdate{DueAt: &past}, f.clock); err != nil {
			t.Error(err)
		}
		return "timeout"
	}

	f.tickAt(10)

	if got := f.deliveryError(r.ID); got != nil {
		t.Errorf("delivery_error = %q after a same-second re-time", *got)
	}
}
