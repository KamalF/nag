// Package notify owns the sweep goroutine (§7.3) and — in M2 — the Web
// Push sender and shoutrrr fan-out it drives.
package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/KamalF/nag/internal/store"
)

const sweepInterval = 30 * time.Second

type Sweep struct {
	store    *store.Store
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time
}

func NewSweep(st *store.Store, log *slog.Logger) *Sweep {
	return &Sweep{store: st, log: log, interval: sweepInterval, now: time.Now}
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
// it (§7.3). M1 runs phase 1 and logs what it would have sent; phases 2–4
// land with their milestones.
func (s *Sweep) tick(ctx context.Context) {
	now := s.now().Unix()

	eligible, tooLate, err := s.store.SweepMark(ctx, now)
	if err != nil {
		s.log.Error("sweep: mark", "error", err)
		return
	}
	for _, id := range tooLate {
		// the row appears in the list and produces no output (§7.3)
		s.log.Info("sweep: marked too late, no notification", "id", id)
	}
	if len(eligible) > 0 {
		// M1 placeholder for phases 2–3: ids only, never text (§10.4)
		s.log.Info("sweep: marked (log-only, sending lands in M2)",
			"count", len(eligible), "ids", markedIDs(eligible))
	}
}

func markedIDs(rows []store.MarkedRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}
