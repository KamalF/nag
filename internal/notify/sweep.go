// Package notify owns the sweep goroutine (§7.3), the Web Push sender it
// drives (M2), and — with M4 — the shoutrrr fan-out.
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

// tooLateSeconds is §7.3's gate: a row more than 30 minutes overdue when
// the sweep first sees it appears in the list and produces no output. The
// same 30 minutes is the M2 push cooldown — one number the user can hold,
// owned here because it is notification policy, not storage: *nothing more
// than half an hour late produces a notification, and no more than one
// notification per half hour.*
const tooLateSeconds = 1800

// maxHeldLatenessSeconds bounds how late a held row may be when the digest
// finally carries it. The phase-1 gate cannot do this on its own: it runs at
// mark time, and a row that passes it is then held for up to a full cooldown
// window — and, after an outage, for however long the server was down (§7.3).
//
// The hour is the arithmetic of the two 30-minute constants rather than a new
// number to remember: a row marked at the gate's edge is already 1800 s late
// and then waits out a window the previous tick consumed, which lands the
// worst legitimate case at 3570 s (pinned by a test). The five minutes on top
// are slack, and they are not decoration — at 3600 the margin is a single
// sweep tick, so two ticks lost to a failing query or a forward clock step
// would silently drop reminders that the two constants allow. This has to
// fail in the direction §7.3 already chose: at worst one extra notification,
// never a missed one.
const maxHeldLatenessSeconds = tooLateSeconds*2 + 300

// fanoutBudget is phase 3's whole-phase deadline (§7.3), measured from a
// fresh clock read when the phase starts — not from the tick's now, which
// sequential pushes to sleeping devices can already have left 30 s behind.
// It is a context threaded into every send, not a check between rows: a
// row of 16 dead channels entering at 19.9 s would otherwise run 160 s
// past it.
const fanoutBudget = 20 * time.Second

// maxDeliveryErrors caps delivery_error at three entries, then "+N more"
// (§4.1), which bounds the column at a couple of hundred bytes.
const maxDeliveryErrors = 3

// perSendTimeout is §7.1's sweep-side deadline. Sends are sequential, so
// three sleeping devices is a 30-second tick and the next one is skipped —
// which the no-overlap rule already tolerates (§7.3).
const perSendTimeout = 10 * time.Second

// pusher is the sweep's view of the §7.1 sender: one send, one verdict.
// The sweep ignores the verdict — nothing a send learns can change whether
// a row has been handled (§7.3) — so this exists to let the phase-2 tests
// run without a push service, not to widen the contract.
type pusher interface {
	SendTo(ctx context.Context, perSend time.Duration, payload []byte, sub store.Subscription) SendResult
}

// channelSender is the sweep's view of notify.SendChannel: one send, one
// verdict — "" or a §4.1 classification. Injectable for the phase-3 tests.
type channelSender func(ctx context.Context, rawURL, message string) string

type Sweep struct {
	store    *store.Store
	log      *slog.Logger
	sender   pusher
	channels channelSender
	budget   time.Duration
	interval time.Duration
	now      func() time.Time

	// lastPushAt is the cooldown's only state, and it is deliberately in
	// memory and not durable (§7.3). It starts at zero, so the tick that
	// runs at boot always finds the window open: a restart opens the
	// window early, which is at worst one extra notification and never a
	// missed one. It is not derived from MAX(pushed_at), because phase 1
	// stamps that column on rows nothing was ever sent about. Only the
	// sweep goroutine touches it.
	lastPushAt int64
}

func NewSweep(st *store.Store, log *slog.Logger, sender pusher) *Sweep {
	return &Sweep{
		store:    st,
		log:      log,
		sender:   sender,
		channels: SendChannel,
		budget:   fanoutBudget,
		interval: sweepInterval,
		now:      time.Now,
	}
}

// Run loops until ctx is cancelled: once immediately at boot, then on the
// ticker. One goroutine on the ticker channel — a tick that runs long
// means the next one is skipped, never queued (§7.3).
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

// tick is one sweep pass. now is read once and used for every statement in
// it (§7.3). Phases 1 and 2 run here; the fan-out and the purge land with
// M4, and the order is load-bearing — every column that decides whether a
// notification happens is committed before any socket is opened.
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
	// after the push, so a third party's dead socket can cost channel
	// messages and never delay this app's own notification (§7.3)
	s.fanOut(ctx, now, eligible)
}

// digest is §7.3 phase 2: with the window open, take the whole held set,
// stamp it, and send one push per subscription. A tick that finds the
// window closed logs what it is holding instead — the line that tells a
// 20-minute silence apart from a broken sweep (§10.4).
func (s *Sweep) digest(ctx context.Context, now int64) {
	// The first half is the backward-clock guard: lastPushAt is in memory and
	// nothing but a restart clears it, so after an NTP step or a snapshot
	// restore a plain difference stays under the cooldown for the whole
	// length of the jump — every reminder marked, held, and never pushed. An
	// unrecognisable window counts as open, which fails in the direction
	// §7.3 already chose for a restart: at worst one extra notification.
	if now >= s.lastPushAt && now-s.lastPushAt < tooLateSeconds {
		s.logHeld(ctx, now)
		return
	}
	// Read the subscriptions before the held set is consumed. The digest
	// transaction stamps pushed_at across the whole held set and commits, and
	// those rows can never be re-selected — so a read that fails after it
	// would drop them for a reason this could have seen first. An empty list
	// is not a failure and does not stop the phase: §7.6 wants the rows
	// stamped and the window consumed even with zero subscriptions stored.
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
		// the same outcome the phase-1 gate produces, reached the other way:
		// stamped, in the list, and silent (§7.3)
		s.log.Info("sweep: held too long, no notification", "id", id)
	}
	if len(held) == 0 {
		// nothing was sent, so the window is untouched — exactly as on a tick
		// where the phase-1 gate stamped every candidate (§7.6)
		return
	}
	// The window is consumed whenever phase 2 selected rows, whatever the
	// sends then do and even with zero subscriptions stored (§7.3): those
	// rows have already left the held set and can never be re-selected,
	// so a window held open on failure would have nothing left to carry.
	s.lastPushAt = now

	if len(subs) == 0 {
		// without this line the first reminders after a fresh deploy
		// vanish with no trace (§7.1)
		s.log.Warn("no push subscriptions stored — the notification goes nowhere",
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
	// the only record of which reminders a given notification was about,
	// which is a question you will have — ids, never texts (§10.4)
	s.log.Info("push sent", "n", len(held), "ids", heldIDs(held), "subscriptions", len(subs))
}

// fanOut is §7.3 phase 3: one message per reminder, to the eligible rows
// phase 1 just marked — gate-stamped rows are not among them, since the
// gate means no output at all — sequentially, under one whole-phase
// deadline. A row's verdict is written as soon as its attempts finish.
// Rows the deadline never reached are dropped with one INFO line and never
// retried: the row is already in "Needs you now", and the list is the
// safety net. Nothing is coalesced or rate-limited here (§7.2).
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
	catalogue, err := s.store.ChannelRows(ctx)
	if err != nil {
		s.log.Error("sweep: read channels", "error", err)
		return
	}
	byName := make(map[string]store.ChannelRow, len(catalogue))
	for _, c := range catalogue {
		byName[c.Name] = c
	}

	// WithTimeout reads the clock afresh: the phase's own start, never the
	// tick's now (§7.3)
	phase, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	for i, r := range rows {
		if phase.Err() != nil {
			s.log.Info("sweep: fan-out deadline reached, rows dropped",
				"count", len(rows)-i, "ids", markedIDs(rows[i:]))
			return
		}
		s.fanOutRow(phase, markedAt, r, byName)
	}
}

// fanOutRow sends one reminder to each of its channels and records the
// verdict. A disabled channel is skipped silently — you muted it yourself —
// while a name that resolves to nothing gets one INFO line, because an
// orphan is the same silence minus any record that you chose it (§7.2).
// Channels the budget never reached record nothing; a send it cut short is
// "timeout", the wrapper's word for attempted and not completed.
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
		attempted++
		if verdict := s.channels(phase, c.URL, message); verdict != "" {
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
		// as the column — never the library's text (§10.4)
		s.log.Info("sweep: fan-out failed", "id", r.ID, "delivery_error", joined)
	}
	// written even when the phase was cancelled mid-row: a column that
	// describes what a socket did is committed once the socket is done
	write, cancel := context.WithTimeout(context.WithoutCancel(phase), deleteTimeout)
	defer cancel()
	if err := s.store.SetDeliveryError(write, r.ID, markedAt, value); err != nil {
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
// actually holds something. A quiet instance stays quiet: with nothing
// held there is no line.
func (s *Sweep) logHeld(ctx context.Context, now int64) {
	held, err := s.store.HeldCount(ctx)
	if err != nil {
		s.log.Error("sweep: held count", "error", err)
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
