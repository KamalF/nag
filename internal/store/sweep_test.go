package store

import (
	"fmt"
	"testing"
)

func TestSweepMarkIdempotent(t *testing.T) {
	s := newStore(t)
	insertRow(t, s, 900, nil, nil, nil)

	eligible, tooLate, err := s.SweepMark(t.Context(), 1000, 1800)
	if err != nil || len(eligible) != 1 || len(tooLate) != 0 {
		t.Fatalf("first mark: %d eligible, %d gated (err %v), want 1, 0", len(eligible), len(tooLate), err)
	}
	got, _ := s.GetReminder(t.Context(), eligible[0].ID)
	if got.NotifiedAt == nil || *got.NotifiedAt != 1000 {
		t.Errorf("notified_at = %v, want 1000", got.NotifiedAt)
	}
	if got.PushedAt != nil {
		t.Errorf("pushed_at = %v, want NULL — the row waits for a digest", got.PushedAt)
	}

	eligible, tooLate, err = s.SweepMark(t.Context(), 1030, 1800)
	if err != nil || len(eligible)+len(tooLate) != 0 {
		t.Errorf("second mark returned rows — a marked row must never fire twice (§7.3)")
	}
}

// §4.1: a backdated write arrives already marked, so phase 1 never sees it.
func TestSweepMarkSkipsBackdatedWrites(t *testing.T) {
	s := newStore(t)
	if _, err := s.CreateReminder(t.Context(), "backdated", 500, nil, 1000); err != nil {
		t.Fatal(err)
	}
	eligible, tooLate, err := s.SweepMark(t.Context(), 1030, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible)+len(tooLate) != 0 {
		t.Error("a backdated create reached phase 1 — the write path must have pre-stamped it")
	}
}

func TestSweepMarkTooLateGate(t *testing.T) {
	s := newStore(t)
	lateID := insertRow(t, s, 1000, nil, nil, nil)     // 2000 s overdue at now=3000
	onTimeID := insertRow(t, s, 2500, nil, nil, nil)   // 500 s overdue
	boundaryID := insertRow(t, s, 1200, nil, nil, nil) // exactly 1800 s overdue: not gated (due_at < now-1800 gates)

	eligible, tooLate, err := s.SweepMark(t.Context(), 3000, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if len(tooLate) != 1 || tooLate[0] != lateID {
		t.Fatalf("tooLate = %v, want [%d]", tooLate, lateID)
	}
	if len(eligible) != 2 || eligible[0].ID != boundaryID || eligible[1].ID != onTimeID {
		t.Fatalf("eligible = %v, want boundary %d then on-time %d (due_at order)", eligible, boundaryID, onTimeID)
	}

	late, _ := s.GetReminder(t.Context(), lateID)
	if late.NotifiedAt == nil || late.PushedAt == nil {
		t.Errorf("gated row stamps = %v/%v, want both set", late.NotifiedAt, late.PushedAt)
	}
	onTime, _ := s.GetReminder(t.Context(), onTimeID)
	if onTime.NotifiedAt == nil || onTime.PushedAt != nil {
		t.Errorf("on-time row stamps = %v/%v, want notified only", onTime.NotifiedAt, onTime.PushedAt)
	}
}

func TestSweepMarkLimit50(t *testing.T) {
	s := newStore(t)
	for i := range 60 {
		insertRow(t, s, int64(2000+i), nil, nil, nil)
	}
	eligible, _, err := s.SweepMark(t.Context(), 3000, 1800)
	if err != nil || len(eligible) != 50 {
		t.Fatalf("first mark = %d rows (err %v), want the LIMIT 50", len(eligible), err)
	}
	eligible, _, err = s.SweepMark(t.Context(), 3030, 1800)
	if err != nil || len(eligible) != 10 {
		t.Fatalf("second mark = %d rows (err %v), want the remaining 10", len(eligible), err)
	}
}

func TestSweepMarkHandsOffTextAndChannels(t *testing.T) {
	s := newStore(t)
	res, err := s.db.Exec(
		`INSERT INTO reminders (text, due_at, created_at, extra_channels) VALUES ('call [bob](https://b)', 900, 1, '["ntfy"]')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	eligible, _, err := s.SweepMark(t.Context(), 1000, 1800)
	if err != nil || len(eligible) != 1 {
		t.Fatal(err)
	}
	got := eligible[0]
	if got.ID != id || got.Text != "call [bob](https://b)" || got.DueAt != 900 ||
		fmt.Sprint(got.ExtraChannels) != "[ntfy]" {
		t.Errorf("hand-off row = %+v — phase 3 never re-queries, so this must be complete", got)
	}
}

func TestSweepMarkSkipsDoneRows(t *testing.T) {
	s := newStore(t)
	insertRow(t, s, 900, nil, nil, ptr(950))
	eligible, tooLate, err := s.SweepMark(t.Context(), 1000, 1800)
	if err != nil || len(eligible)+len(tooLate) != 0 {
		t.Error("a done row reached phase 1")
	}
}
