package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Reminder is one row of the §4 reminders table. Pointer fields are the
// NULLable columns. ExtraChannels is the decoded JSON array — always
// canonical (de-duplicated, sorted) because §8.3 canonicalises before
// every write.
type Reminder struct {
	ID            int64
	Text          string
	DueAt         int64
	NotifiedAt    *int64
	PushedAt      *int64
	DoneAt        *int64
	CreatedAt     int64
	ExtraChannels []string
	DeliveryError *string
}

const reminderColumns = "id, text, due_at, notified_at, pushed_at, done_at, created_at, extra_channels, delivery_error"

// backdatedStamps is §4.1's born-notified rule, shared by create and
// re-time: a due_at that is not in the future stamps both lifecycle
// columns to now, so the row is invisible to the sweep's phase 1 and can
// never become a digest candidate.
func backdatedStamps(dueAt, now int64) (notified, pushed *int64) {
	if dueAt <= now {
		return &now, &now
	}
	return nil, nil
}

// CreateReminder inserts a reminder. When dueAt is not after now, the same
// statement stamps notified_at = pushed_at = now (§4.1): a backdated write
// appears in the overdue list immediately and never notifies.
func (s *Store) CreateReminder(ctx context.Context, text string, dueAt int64, extraChannels []string, now int64) (Reminder, error) {
	extraChannels = canonicalChannelList(extraChannels)
	notified, pushed := backdatedStamps(dueAt, now)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO reminders (text, due_at, notified_at, pushed_at, done_at, created_at, extra_channels) VALUES (?, ?, ?, ?, NULL, ?, ?)",
		text, dueAt, notified, pushed, now, encodeChannels(extraChannels))
	if err != nil {
		return Reminder{}, fmt.Errorf("insert reminder: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Reminder{}, err
	}
	return s.GetReminder(ctx, id)
}

// GetReminder returns the row, or sql.ErrNoRows.
func (s *Store) GetReminder(ctx context.Context, id int64) (Reminder, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+reminderColumns+" FROM reminders WHERE id = ?", id)
	return scanReminder(row)
}

// ListPending returns every un-cleared reminder, sorted by due_at then id
// (§8.2): the id tiebreak is what keeps two same-minute rows from swapping
// places between polls.
func (s *Store) ListPending(ctx context.Context) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+reminderColumns+" FROM reminders WHERE done_at IS NULL ORDER BY due_at ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Channel is the client-visible half of a channels row. The URL never
// reaches the API layer in any form (§4.1) — the send path gets its own
// accessor when it lands.
type Channel struct {
	Name    string
	Enabled bool
}

// ListChannels returns every channel in name order (§8.2) — chip order in
// the UI, never re-sorted client-side.
func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, enabled FROM channels ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.Name, &c.Enabled); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReminderUpdate is one PATCH's writes. nil means "leave unchanged";
// ExtraChannels pointing at an empty slice means "clear the list" —
// presence and null are different answers in §8.3.
type ReminderUpdate struct {
	Text          *string
	DueAt         *int64
	ExtraChannels *[]string
}

// UpdateReminder applies u to current — the row as the caller just read it
// (one read, one pre-image) — in one UPDATE (§4.1). A DueAt is a re-time:
// notified_at and pushed_at reset (or stamp to now on a backdated value,
// exactly as on create), done_at clears, delivery_error clears. A changed
// ExtraChannels list — an ordered comparison of the canonical forms — also
// clears delivery_error; an identical list touches nothing, so re-saving
// a row without changing its channels is not a clear.
func (s *Store) UpdateReminder(ctx context.Context, current Reminder, u ReminderUpdate, now int64) (Reminder, error) {
	var set []string
	var args []any
	if u.Text != nil {
		set = append(set, "text = ?")
		args = append(args, *u.Text)
	}
	if u.DueAt != nil {
		notified, pushed := backdatedStamps(*u.DueAt, now)
		set = append(set, "due_at = ?", "notified_at = ?", "pushed_at = ?",
			"done_at = NULL", "delivery_error = NULL")
		args = append(args, *u.DueAt, notified, pushed)
	}
	if u.ExtraChannels != nil {
		canonical := canonicalChannelList(*u.ExtraChannels)
		if !slices.Equal(current.ExtraChannels, canonical) {
			set = append(set, "extra_channels = ?", "delivery_error = NULL")
			args = append(args, encodeChannels(canonical))
		}
	}
	if len(set) == 0 {
		return current, nil
	}
	args = append(args, current.ID)
	_, err := s.db.ExecContext(ctx,
		"UPDATE reminders SET "+strings.Join(set, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return Reminder{}, err
	}
	return s.GetReminder(ctx, current.ID)
}

// MarkDone stamps done_at = now and returns the row. On a row that is
// already done it is a successful no-op keeping the original done_at
// (§8.2): re-stamping would push the retention clock forward on every
// double-tap. sql.ErrNoRows when the id does not exist.
func (s *Store) MarkDone(ctx context.Context, id, now int64) (Reminder, error) {
	_, err := s.db.ExecContext(ctx,
		"UPDATE reminders SET done_at = ? WHERE id = ? AND done_at IS NULL", now, id)
	if err != nil {
		return Reminder{}, err
	}
	return s.GetReminder(ctx, id)
}

// MarkUndone clears done_at, and in the same statement stamps
// pushed_at = now exactly when notified_at IS NOT NULL AND pushed_at IS
// NULL (§4.1): a row cleared during a cooldown was dropped from the held
// set by done_at, and un-clearing it later must not walk it back into a
// digest — while a row cleared before it ever fired keeps its NULLs and
// returns as a live phase-1 candidate. On a row that isn't done it is a
// successful no-op. sql.ErrNoRows when the id does not exist.
func (s *Store) MarkUndone(ctx context.Context, id, now int64) (Reminder, error) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE reminders SET
		  done_at = NULL,
		  pushed_at = CASE
		    WHEN notified_at IS NOT NULL AND pushed_at IS NULL THEN ?
		    ELSE pushed_at
		  END
		WHERE id = ? AND done_at IS NOT NULL`, now, id)
	if err != nil {
		return Reminder{}, err
	}
	return s.GetReminder(ctx, id)
}

// DeleteReminder hard-deletes the row (§8.2's curl affordance). It reports
// whether a row existed.
func (s *Store) DeleteReminder(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM reminders WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

type scannable interface {
	Scan(dest ...any) error
}

func scanReminder(row scannable) (Reminder, error) {
	var r Reminder
	var notified, pushed, done sql.NullInt64
	var channels, deliveryError sql.NullString
	err := row.Scan(&r.ID, &r.Text, &r.DueAt, &notified, &pushed, &done,
		&r.CreatedAt, &channels, &deliveryError)
	if err != nil {
		return Reminder{}, err
	}
	r.NotifiedAt = nullableInt(notified)
	r.PushedAt = nullableInt(pushed)
	r.DoneAt = nullableInt(done)
	if deliveryError.Valid {
		r.DeliveryError = &deliveryError.String
	}
	if channels.Valid {
		r.ExtraChannels, err = decodeChannels(r.ID, channels.String)
		if err != nil {
			return Reminder{}, err
		}
	}
	return r, nil
}

func nullableInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// canonicalChannelList is the one stored form of a channel set (§8.3):
// de-duplicated and sorted, so §4.1's "the list differs" test is a plain
// ordered comparison. The store owns this invariant — its own comparison
// in UpdateReminder depends on it, whichever caller writes.
func canonicalChannelList(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	canonical := slices.Clone(names)
	slices.Sort(canonical)
	return slices.Compact(canonical)
}

// encodeChannels stores an empty list as NULL — §4's column is nullable
// and §8.2 re-materialises [] on the way out.
func encodeChannels(names []string) *string {
	if len(names) == 0 {
		return nil
	}
	raw, _ := json.Marshal(names)
	s := string(raw)
	return &s
}

// decodeChannels is encodeChannels' twin, shared by every scanner of the
// column.
func decodeChannels(id int64, raw string) ([]string, error) {
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, fmt.Errorf("reminder %d: extra_channels: %w", id, err)
	}
	return names, nil
}
