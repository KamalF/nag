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

// Phase 2 selects the held set in due order and stamps exactly that set.
// The predicate-not-id-list UPDATE must not reach a done row or an unmarked
// one. It must reach every held row, however many there are (§7.3).
func TestSweepCollectDigestStampsExactlyTheHeldSet(t *testing.T) {
	s := newStore(t)
	late := insertRow(t, s, 2000, ptr(int64(2100)), nil, nil)
	early := insertRow(t, s, 1000, ptr(int64(2100)), nil, nil)
	cleared := insertRow(t, s, 1500, ptr(int64(2100)), nil, ptr(int64(2200)))
	unmarked := insertRow(t, s, 9000, nil, nil, nil)

	held, tooLate, err := s.SweepCollectDigest(t.Context(), 3000, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(tooLate) != 0 {
		t.Fatalf("tooLate = %v, want none — both rows are inside the bound", tooLate)
	}
	if len(held) != 2 || held[0].ID != early || held[1].ID != late {
		t.Fatalf("held = %v, want [%d %d] in due order", held, early, late)
	}
	for _, id := range []int64{early, late} {
		got, _ := s.GetReminder(t.Context(), id)
		if got.PushedAt == nil || *got.PushedAt != 3000 {
			t.Errorf("id %d: pushed_at = %v, want 3000", id, got.PushedAt)
		}
	}
	for _, id := range []int64{cleared, unmarked} {
		got, _ := s.GetReminder(t.Context(), id)
		if got.PushedAt != nil {
			t.Errorf("id %d was stamped by the predicate UPDATE: pushed_at = %v", id, got.PushedAt)
		}
	}

	// the rows have left the held set for good
	again, _, err := s.SweepCollectDigest(t.Context(), 4000, 3600)
	if err != nil || len(again) != 0 {
		t.Errorf("second collect returned %d rows (err %v), want none", len(again), err)
	}
}

func TestHeldCountCountsOnlyWaitingRows(t *testing.T) {
	s := newStore(t)
	insertRow(t, s, 1000, ptr(int64(1100)), nil, nil)              // held
	insertRow(t, s, 1000, ptr(int64(1100)), ptr(int64(1100)), nil) // already pushed
	insertRow(t, s, 1000, ptr(int64(1100)), nil, ptr(int64(1200))) // cleared while held
	insertRow(t, s, 9000, nil, nil, nil)                           // not yet due

	n, err := s.HeldCount(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("held count = %d (err %v), want 1", n, err)
	}
}

// The age split decides what is sent, never what is stamped. A row too late
// to notify about still leaves the held set. Otherwise the next tick finds
// it again, at an age that phase 2 already refused (§7.3).
func TestSweepCollectDigestStampsTheRowsItRefusesToSend(t *testing.T) {
	s := newStore(t)
	stranded := insertRow(t, s, 1000, ptr(int64(1100)), nil, nil) // 9000 s late at now=10000
	fresh := insertRow(t, s, 9500, ptr(int64(9600)), nil, nil)    // 500 s late
	boundary := insertRow(t, s, 6400, ptr(int64(6500)), nil, nil) // exactly 3600 s late: kept

	held, tooLate, err := s.SweepCollectDigest(t.Context(), 10000, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(tooLate) != 1 || tooLate[0] != stranded {
		t.Fatalf("tooLate = %v, want [%d]", tooLate, stranded)
	}
	if len(held) != 2 || held[0].ID != boundary || held[1].ID != fresh {
		t.Fatalf("held = %v, want [%d %d] in due order — the boundary row is inside the bound",
			held, boundary, fresh)
	}
	for _, id := range []int64{stranded, fresh, boundary} {
		got, _ := s.GetReminder(t.Context(), id)
		if got.PushedAt == nil || *got.PushedAt != 10000 {
			t.Errorf("id %d: pushed_at = %v, want 10000 — every held row is stamped", id, got.PushedAt)
		}
	}
}
