// Package notify owns the sweep goroutine (§7.3), the Web Push sender that
// it drives (§7.1), and the shoutrrr fan-out (§7.2).
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/KamalF/nag/internal/store"
)

const sweepInterval = 30 * time.Second

// tooLateSeconds is the gate of §7.3. If a row is more than 30 minutes
// overdue when the sweep first sees it, the row appears in the list and
// produces no output. The same 30 minutes is the push cooldown: one
// number that the user can hold. This package owns it because it is
// notification policy, not storage: *nothing more than half an hour late
// produces a notification, and no more than one notification per half
// hour.*
const tooLateSeconds = 1800

// maxHeldLatenessSeconds bounds how late a held row can be when the digest
// finally carries it. The phase-1 gate cannot do this on its own, because it
// runs at mark time. A row that passes the gate is then held for up to a
// full cooldown window. After an outage, it is also held for however long
// the server was down (§7.3).
//
// The base hour comes from the two 30-minute constants, so it is not a new
// number to remember. A row marked at the gate's edge is already 1800 s
// late. It then waits until the end of a window that the previous tick
// consumed. That puts the worst legitimate case at 3570 s (pinned by a
// test). The five minutes on top are slack, and the bound needs them. With
// a bound of 3600 s, the margin is a single sweep tick. Two ticks lost to a failing query or a forward clock
// step then silently drop reminders that the two constants allow. This bound
// must fail in the direction that §7.3 already chose: at worst one extra
// notification, never a missed one.
const maxHeldLatenessSeconds = tooLateSeconds*2 + 300

// fanoutBudget is the whole-phase deadline of phase 3 (§7.3). It is measured
// from a fresh clock read when the phase starts, not from the tick's now.
// Sequential pushes to sleeping devices can already have put that now 30 s
// in the past. The budget is a context passed into every send, not a check
// between rows. With only a check between rows, a row of 16 dead channels
// that enters at 19.9 s runs 160 s past the budget.
const fanoutBudget = 20 * time.Second

// maxDeliveryErrors caps delivery_error at three entries, then "+N more"
// (§4.1), which bounds the column at a couple of hundred bytes.
const maxDeliveryErrors = 3

// perSendTimeout is the sweep-side deadline of §7.1. Sends are sequential,
// so three sleeping devices make a 30-second tick, and the next tick is
// skipped. The no-overlap rule already tolerates that (§7.3).
const perSendTimeout = 10 * time.Second

// pusher is the sweep's view of the §7.1 sender: one send, one verdict.
// The sweep ignores the verdict, because nothing that a send learns can
// change whether a row is handled (§7.3). So this interface exists to let
// the phase-2 tests run without a push service. It does not give the sweep a
// use for the verdict.
type pusher interface {
	SendTo(ctx context.Context, perSend time.Duration, payload []byte, sub store.Subscription) SendResult
}

// channelSender is the sweep's view of notify.SendChannel: one send, one
// verdict, "" or a §4.1 classification. It is injectable for the phase-3
// tests.
type channelSender func(ctx context.Context, rawURL, message string) string

type Sweep struct {
	store    *store.Store
	log      *slog.Logger
	sender   pusher
	channels channelSender
	budget   time.Duration
	interval time.Duration
	now      func() time.Time

	// retentionDays is read once per tick, from whatever config is
	// current: the only value that the sweep takes from the TOML (§4.4, §5.5).
	retentionDays func() int

	// lastPushAt is the cooldown's only state. It is deliberately in memory
	// and not durable (§7.3). It starts at zero, so the tick that runs at
	// boot always finds the window open. As a result, a restart opens the window
	// early: at worst one extra notification, and never a missed one. It is
	// not derived from MAX(pushed_at), because phase 1 stamps that column on
	// rows that nothing was ever sent about. Only the sweep goroutine
	// touches it.
	lastPushAt int64
}

func NewSweep(st *store.Store, log *slog.Logger, sender pusher, retentionDays func() int) *Sweep {
	return &Sweep{
		store:         st,
		log:           log,
		sender:        sender,
		retentionDays: retentionDays,
		channels:      SendChannel,
		budget:        fanoutBudget,
		interval:      sweepInterval,
		now:           time.Now,
	}
}

// Run loops until ctx is canceled: once immediately at boot, then on the
// ticker. One goroutine reads the ticker channel. So if a tick runs long,
// the next one is skipped, never queued (§7.3).
func (s *Sweep) Run(ctx context.Context) {
	s.tick(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick is one sweep pass. It reads now once and uses it for every statement
// in the pass (§7.3). It runs the four phases: mark, digest, fan-out and
// purge. The order matters: every column that decides
// whether a notification happens is committed before any socket opens.
func (s *Sweep) tick(ctx context.Context) {
	now := s.now().Unix()

	eligible, tooLate, err := s.store.SweepMark(ctx, now, tooLateSeconds)
	if err != nil {
		s.log.Error("sweep: mark", "error", err)
		return
	}
	for _, id := range tooLate {
		// the row appears in the list and produces no output (§7.3)
		s.log.Info("sweep: marked too late, no notification", "id", id)
	}
	if len(eligible) > 0 {
		// ids only, never text (§10.4)
		s.log.Info("sweep: marked", "count", len(eligible), "ids", markedIDs(eligible))
	}

	s.digest(ctx, now)
	// fan-out runs after the push, so a dead socket of a third party can
	// cost channel messages but never delays the push of this app (§7.3)
	s.fanOut(ctx, now, eligible)
	if ctx.Err() != nil {
		return // shutting down: the purge can wait for the next boot
	}
	s.purge(ctx, now)
}

// purge is §7.3 phase 4 (§4.4): it deletes cleared rows older than
// retention_days.
// Zero means keep them forever, and the DELETE is skipped entirely.
func (s *Sweep) purge(ctx context.Context, now int64) {
	days := s.retentionDays()
	if days == 0 {
		return
	}
	n, err := s.store.Purge(ctx, now-int64(days)*86400)
	if err != nil {
		s.log.Error("sweep: purge", "error", err)
		return
	}
	if n > 0 {
		s.log.Info("sweep: purged cleared reminders", "count", n, "retention_days", days)
	}
}

// digest is §7.3 phase 2. If the window is open, it takes the whole held
// set, stamps it, and sends one push per subscription. If a tick finds the
// window closed, it logs what it is holding instead. That line tells a
// 20-minute silence apart from a broken sweep (§10.4).
func (s *Sweep) digest(ctx context.Context, now int64) {
	// The first half of the condition is the backward-clock guard.
	// lastPushAt is in memory, and only a restart clears it. After a
	// backward NTP step or a snapshot restore, a plain difference stays under
	// the cooldown for the whole length of the jump. Every reminder is then
	// marked, held, and never pushed. If now is earlier than lastPushAt, the
	// window counts as open. That fails in the direction that
	// §7.3 already chose for a restart: at worst one extra notification.
	if now >= s.lastPushAt && now-s.lastPushAt < tooLateSeconds {
		s.logHeld(ctx, now)
		return
	}
	// Read the subscriptions before the held set is consumed. The digest
	// transaction stamps pushed_at across the whole held set and commits.
	// Those rows can never be re-selected. If the read runs after the commit
	// and fails, those rows are lost. If the read runs first, its failure
	// stops the phase before any row is stamped. An empty list is not a
	// failure and does not stop the phase. §7.6 requires the rows stamped
	// and the window consumed even with zero subscriptions stored.
	subs, err := s.store.ListSubscriptions(ctx)
	if err != nil {
		s.log.Error("sweep: list subscriptions", "error", err)
		return
	}
	held, tooLate, err := s.store.SweepCollectDigest(ctx, now, maxHeldLatenessSeconds)
	if err != nil {
		s.log.Error("sweep: collect digest", "error", err)
		return
	}
	for _, id := range tooLate {
		// the same outcome as the phase-1 gate, reached in phase 2 instead:
		// stamped, in the list, and silent (§7.3)
		s.log.Info("sweep: held too long, no notification", "id", id)
	}
	if len(held) == 0 {
		// nothing was sent, so the window is not consumed, exactly as on a tick
		// where the phase-1 gate stamped every candidate (§7.6)
		return
	}
	// The window is consumed whenever phase 2 selected rows, whatever the
	// sends then do, and even with zero subscriptions stored (§7.3). Those
	// rows already left the held set and can never be re-selected. So if the
	// window stayed open after a failure, it has no rows left to carry.
	s.lastPushAt = now

	if len(subs) == 0 {
		// without this line, the first reminders after a fresh deploy
		// vanish with no trace (§7.1)
		s.log.Warn("sweep: no push subscriptions stored — the notification goes nowhere",
			"count", len(held), "first_id", held[0].ID)
		return
	}
	payload, err := buildDigestPayload(held)
	if err != nil {
		s.log.Error("sweep: build payload", "error", err)
		return
	}
	for _, sub := range subs {
		// one call per subscription, not per reminder: the digest is what
		// keeps a backlog from becoming a burst (§7.3)
		s.sender.SendTo(ctx, perSendTimeout, payload, sub)
	}
	// this line is the only record of which reminders a notification was
	// about, and you will ask that question. It names ids, never texts (§10.4)
	s.log.Info("push sent", "n", len(held), "ids", heldIDs(held), "subscriptions", len(subs))
}

// fanOut is §7.3 phase 3. It sends one message per reminder, sequentially,
// under one whole-phase deadline, to the eligible rows that phase 1 just
// marked. Gate-stamped rows are not among them, because the gate means no
// output at all. A row's verdict is written as soon as its attempts finish.
// Rows that phase 3 did not reach before the deadline are dropped with one
// INFO line and never retried. Each such row is already in "Needs you now",
// and the list is the safety net. Nothing is coalesced or rate-limited here (§7.2).
func (s *Sweep) fanOut(ctx context.Context, markedAt int64, eligible []store.MarkedRow) {
	var rows []store.MarkedRow
	for _, r := range eligible {
		if len(r.ExtraChannels) > 0 {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return
	}
	catalog, err := s.store.ChannelRows(ctx)
	if err != nil {
		s.log.Error("sweep: read channels", "error", err)
		return
	}
	byName := make(map[string]store.ChannelRow, len(catalog))
	for _, c := range catalog {
		byName[c.Name] = c
	}

	// WithTimeout reads the clock again, so the budget counts from the start
	// of this phase, never from the tick's now (§7.3)
	phase, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	for i, r := range rows {
		if phase.Err() != nil {
			reason := "sweep: fan-out deadline reached, rows dropped"
			if ctx.Err() != nil {
				reason = "sweep: fan-out stopped by shutdown, rows dropped"
			}
			s.log.Info(reason, "count", len(rows)-i, "ids", markedIDs(rows[i:]))
			return
		}
		s.fanOutRow(phase, markedAt, r, byName)
	}
}

// fanOutRow sends one reminder to each of its channels and records the
// verdict. A disabled channel is skipped silently, because you muted it
// yourself. A name that resolves to nothing gets one INFO line, because an
// orphan gives the same silence, but with no record that you chose it (§7.2).
// Channels that the budget never reached record nothing. A send that the
// budget cut short is "timeout", the wrapper's word for attempted and not
// completed.
func (s *Sweep) fanOutRow(phase context.Context, markedAt int64, r store.MarkedRow, byName map[string]store.ChannelRow) {
	message := flattenLinks(r.Text) // a channel is an output, not a renderer
	var failures []string
	attempted := 0
	for _, name := range r.ExtraChannels {
		c, known := byName[name]
		switch {
		case !known:
			s.log.Info("sweep: channel no longer exists, skipped", "id", r.ID, "channel", name)
			continue
		case !c.Enabled:
			continue
		case phase.Err() != nil:
			// the budget ran out partway through this row: nothing was
			// attempted for the rest, so nothing is recorded for them
			continue
		}
		verdict := s.channels(phase, c.URL, message)
		if verdict == NotAttempted {
			// an earlier send to this URL is still stuck: nothing was
			// attempted, so nothing is recorded, but the silence is logged
			s.log.Info("sweep: channel still busy with an earlier send, skipped", "id", r.ID, "channel", name)
			continue
		}
		attempted++
		if verdict != "" {
			failures = append(failures, name+": "+verdict)
		}
	}
	if attempted == 0 {
		return // the column only ever describes attempts (§4.1)
	}
	var value *string
	if len(failures) > 0 {
		joined := formatDeliveryError(failures)
		value = &joined
		// every delivery failure gets a line, in the same classified form
		// as the column, never the library's text (§10.4)
		s.log.Info("sweep: fan-out failed", "id", r.ID, "delivery_error", joined)
	}
	// written even when the phase was canceled mid-row: a column that
	// describes what a socket did is committed once the socket is done
	write, cancel := context.WithTimeout(context.WithoutCancel(phase), deleteTimeout)
	defer cancel()
	if err := s.store.SetDeliveryError(write, r, markedAt, value); err != nil {
		s.log.Error("sweep: write delivery_error", "id", r.ID, "error", err)
	}
}

// formatDeliveryError joins at most three classified entries with "; ",
// then "; +N more" (§4.1).
func formatDeliveryError(failures []string) string {
	if len(failures) <= maxDeliveryErrors {
		return strings.Join(failures, "; ")
	}
	return strings.Join(failures[:maxDeliveryErrors], "; ") +
		fmt.Sprintf("; +%d more", len(failures)-maxDeliveryErrors)
}

// logHeld writes the §10.4 held-rows line, one short line per tick that
// actually holds something. A quiet instance stays quiet: if nothing is
// held, there is no line.
func (s *Sweep) logHeld(ctx context.Context, now int64) {
	held, err := s.store.HeldCount(ctx)
	if err != nil {
		s.log.Error("sweep: count held rows", "error", err)
		return
	}
	if held == 0 {
		return
	}
	s.log.Info("sweep: holding rows until the cooldown window opens",
		"held", held, "seconds_remaining", s.lastPushAt+tooLateSeconds-now)
}

func markedIDs(rows []store.MarkedRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func heldIDs(rows []store.HeldRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}
