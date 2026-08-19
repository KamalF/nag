package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "nag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// insertRow writes a reminder with exact stamps — the sweep that produces
// the fired-and-held shape lands in a later commit, and these tests pin
// §4.1 against every shape it will produce.
func insertRow(t *testing.T, s *Store, dueAt int64, notified, pushed, done *int64) int64 {
	t.Helper()
	res, err := s.db.Exec(
		"INSERT INTO reminders (text, due_at, notified_at, pushed_at, done_at, created_at) VALUES ('t', ?, ?, ?, ?, 1)",
		dueAt, notified, pushed, done)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func ptr(v int64) *int64 { return &v }

func TestMarkDoneKeepsOriginalStamp(t *testing.T) {
	s := newStore(t)
	id := insertRow(t, s, 10, nil, nil, nil)

	first, err := s.MarkDone(t.Context(), id, 100)
	if err != nil || first.DoneAt == nil || *first.DoneAt != 100 {
		t.Fatalf("first done = %v (err %v), want done_at 100", first.DoneAt, err)
	}
	second, err := s.MarkDone(t.Context(), id, 200)
	if err != nil {
		t.Fatal(err)
	}
	if second.DoneAt == nil || *second.DoneAt != 100 {
		t.Errorf("double-tap moved done_at to %v, want the original 100 (§8.2 retention argument)", second.DoneAt)
	}
}

func TestMarkUndoneGuard(t *testing.T) {
	s := newStore(t)

	t.Run("cleared before it ever fired: stamps stay NULL", func(t *testing.T) {
		id := insertRow(t, s, 10, nil, nil, ptr(60))
		got, err := s.MarkUndone(t.Context(), id, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.DoneAt != nil {
			t.Error("done_at not cleared")
		}
		if got.NotifiedAt != nil || got.PushedAt != nil {
			t.Errorf("stamps = %v/%v, want NULL/NULL — the row must return as a live phase-1 candidate (§4.1)",
				got.NotifiedAt, got.PushedAt)
		}
	})

	t.Run("fired and held: pushed_at is stamped", func(t *testing.T) {
		id := insertRow(t, s, 10, ptr(50), nil, ptr(60))
		got, err := s.MarkUndone(t.Context(), id, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.DoneAt != nil {
			t.Error("done_at not cleared")
		}
		if got.PushedAt == nil || *got.PushedAt != 100 {
			t.Errorf("pushed_at = %v, want 100 — undone must not walk the row back into a digest (§4.1)",
				got.PushedAt)
		}
	})

	t.Run("already pushed about: both stamps stay", func(t *testing.T) {
		id := insertRow(t, s, 10, ptr(50), ptr(55), ptr(60))
		got, err := s.MarkUndone(t.Context(), id, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.PushedAt == nil || *got.PushedAt != 55 {
			t.Errorf("pushed_at = %v, want the original 55", got.PushedAt)
		}
	})

	t.Run("no-op on a row that isn't done", func(t *testing.T) {
		id := insertRow(t, s, 10, ptr(50), nil, nil) // held by the cooldown
		got, err := s.MarkUndone(t.Context(), id, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.PushedAt != nil {
			t.Errorf("no-op undone stamped pushed_at = %v — the row must stay in the held set", got.PushedAt)
		}
	})
}

func TestLifecycleUnknownID(t *testing.T) {
	s := newStore(t)
	if _, err := s.MarkDone(t.Context(), 9999, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("MarkDone unknown id: err = %v, want ErrNoRows", err)
	}
	if _, err := s.MarkUndone(t.Context(), 9999, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("MarkUndone unknown id: err = %v, want ErrNoRows", err)
	}
}

func TestDeleteReminder(t *testing.T) {
	s := newStore(t)
	id := insertRow(t, s, 10, nil, nil, nil)

	existed, err := s.DeleteReminder(t.Context(), id)
	if err != nil || !existed {
		t.Fatalf("delete = %v, %v; want true, nil", existed, err)
	}
	if _, err := s.GetReminder(t.Context(), id); !errors.Is(err, sql.ErrNoRows) {
		t.Error("row still present after delete")
	}
	existed, err = s.DeleteReminder(t.Context(), id)
	if err != nil || existed {
		t.Errorf("second delete = %v, %v; want false, nil", existed, err)
	}
}
