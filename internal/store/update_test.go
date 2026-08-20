package store

import (
	"database/sql"
	"errors"
	"testing"
)

// mustGet reads the pre-image UpdateReminder now takes — the row as the
// caller just read it.
func mustGet(t *testing.T, s *Store, id int64) Reminder {
	t.Helper()
	r, err := s.GetReminder(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func setDeliveryError(t *testing.T, s *Store, id int64, msg string) {
	t.Helper()
	if _, err := s.db.Exec("UPDATE reminders SET delivery_error = ? WHERE id = ?", msg, id); err != nil {
		t.Fatal(err)
	}
}

func setChannels(t *testing.T, s *Store, id int64, channelsJSON string) {
	t.Helper()
	if _, err := s.db.Exec("UPDATE reminders SET extra_channels = ? WHERE id = ?", channelsJSON, id); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateReTime(t *testing.T) {
	s := newStore(t)

	t.Run("future re-time resets stamps, un-clears, clears delivery_error", func(t *testing.T) {
		id := insertRow(t, s, 10, ptr(50), ptr(55), ptr(60))
		setDeliveryError(t, s, id, "ntfy: timeout")

		due := int64(5000)
		got, err := s.UpdateReminder(t.Context(), mustGet(t, s, id), ReminderUpdate{DueAt: &due}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.DueAt != 5000 || got.NotifiedAt != nil || got.PushedAt != nil || got.DoneAt != nil {
			t.Errorf("re-time left %+v, want due 5000 and all stamps NULL (§4.1)", got)
		}
		if got.DeliveryError != nil {
			t.Errorf("delivery_error = %v, want cleared on re-time", *got.DeliveryError)
		}
	})

	t.Run("backdated re-time stamps both, exactly as on create", func(t *testing.T) {
		id := insertRow(t, s, 9000, nil, nil, nil)
		due := int64(50)
		got, err := s.UpdateReminder(t.Context(), mustGet(t, s, id), ReminderUpdate{DueAt: &due}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.NotifiedAt == nil || *got.NotifiedAt != 100 || got.PushedAt == nil || *got.PushedAt != 100 {
			t.Errorf("stamps = %v/%v, want 100/100", got.NotifiedAt, got.PushedAt)
		}
	})
}

func TestUpdateChannelsClearRule(t *testing.T) {
	s := newStore(t)

	t.Run("a changed list clears delivery_error", func(t *testing.T) {
		id := insertRow(t, s, 10, nil, nil, nil)
		setChannels(t, s, id, `["ntfy"]`)
		setDeliveryError(t, s, id, "ntfy: timeout")

		got, err := s.UpdateReminder(t.Context(), mustGet(t, s, id),
			ReminderUpdate{ExtraChannels: &[]string{"ntfy", "telegram"}}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.DeliveryError != nil {
			t.Error("delivery_error survived a channel change (§4.1)")
		}
		if len(got.ExtraChannels) != 2 {
			t.Errorf("extra_channels = %v", got.ExtraChannels)
		}
	})

	t.Run("an identical list clears nothing", func(t *testing.T) {
		id := insertRow(t, s, 10, nil, nil, nil)
		setChannels(t, s, id, `["ntfy"]`)
		setDeliveryError(t, s, id, "ntfy: timeout")

		got, err := s.UpdateReminder(t.Context(), mustGet(t, s, id),
			ReminderUpdate{ExtraChannels: &[]string{"ntfy"}}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.DeliveryError == nil {
			t.Error("re-saving the same list cleared delivery_error — §4.1's ordered comparison must say unchanged")
		}
	})

	t.Run("clearing the list clears delivery_error", func(t *testing.T) {
		id := insertRow(t, s, 10, nil, nil, nil)
		setChannels(t, s, id, `["ntfy"]`)
		setDeliveryError(t, s, id, "ntfy: refused")

		got, err := s.UpdateReminder(t.Context(), mustGet(t, s, id),
			ReminderUpdate{ExtraChannels: &[]string{}}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.DeliveryError != nil || len(got.ExtraChannels) != 0 {
			t.Errorf("got %+v, want empty channels and no delivery_error", got)
		}
	})
}

// A row deleted between the caller's read and the update surfaces as
// ErrNoRows, which the PATCH handler maps to §8.3's unknown-{id} 404.
func TestUpdateVanishedRowIsErrNoRows(t *testing.T) {
	s := newStore(t)
	id := insertRow(t, s, 10, nil, nil, nil)
	current := mustGet(t, s, id)
	if _, err := s.DeleteReminder(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	text := "too late"
	_, err := s.UpdateReminder(t.Context(), current, ReminderUpdate{Text: &text}, 100)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want ErrNoRows", err)
	}
}

func TestUpdateTextOnlyLeavesLifecycleAlone(t *testing.T) {
	s := newStore(t)
	id := insertRow(t, s, 10, ptr(50), ptr(55), ptr(60))
	text := "new text"
	got, err := s.UpdateReminder(t.Context(), mustGet(t, s, id), ReminderUpdate{Text: &text}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "new text" {
		t.Errorf("text = %q", got.Text)
	}
	if got.NotifiedAt == nil || got.PushedAt == nil || got.DoneAt == nil {
		t.Errorf("text-only update touched lifecycle stamps: %+v", got)
	}
}
