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
// NULLable columns. ExtraChannels is the decoded JSON array. It is always
// canonical (de-duplicated, sorted), because §8.3 canonicalizes before every
// write.
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

// backdatedStamps is §4.1's rule for a due_at in the past, shared by create
// and re-time. A due_at that is not in the future stamps both lifecycle columns
// to now. The row is then invisible to the sweep's phase 1, and can never
// become a digest candidate.
func backdatedStamps(dueAt, now int64) (notified, pushed *int64) {
	if dueAt <= now {
		return &now, &now
	}
	return nil, nil
}

// CreateReminder inserts a reminder. If dueAt is not after now, the same
// statement stamps notified_at = pushed_at = now (§4.1). As a result, a
// backdated write appears in the overdue list immediately and never
// notifies.
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
// (§8.2). The id tiebreak keeps two same-minute rows from swapping places
// between polls.
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
// reaches the API layer in any form (§4.1). The send path reads ChannelRow
// instead.
type Channel struct {
	Name    string
	Enabled bool
}

// ListChannels returns every channel in name order (§8.2). This is the chip
// order in the UI, and the client never re-sorts it.
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

// ReminderUpdate is one PATCH's writes. nil means "leave unchanged".
// ExtraChannels that points at an empty slice means "clear the list": §8.3
// gives different answers for an absent key and for null.
type ReminderUpdate struct {
	Text          *string
	DueAt         *int64
	ExtraChannels *[]string
}

// UpdateReminder applies u to current in one UPDATE (§4.1). current is the
// row as the caller just read it, so the caller and UpdateReminder use the
// same single read of the row. A non-nil DueAt is a re-time: notified_at and
// pushed_at reset (or stamp to now on a backdated value, exactly as on
// create), and done_at and delivery_error clear. A changed ExtraChannels list
// also clears delivery_error. "Changed" is an ordered comparison of the
// canonical forms. An identical list touches nothing, so a re-save of a row
// with the same channels does not clear delivery_error.
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
// already done, it is a successful no-op that keeps the original done_at
// (§8.2). A second stamp moves the retention clock forward on every
// double-tap. It returns sql.ErrNoRows if the id does not exist.
func (s *Store) MarkDone(ctx context.Context, id, now int64) (Reminder, error) {
	_, err := s.db.ExecContext(ctx,
		"UPDATE reminders SET done_at = ? WHERE id = ? AND done_at IS NULL", now, id)
	if err != nil {
		return Reminder{}, err
	}
	return s.GetReminder(ctx, id)
}

// MarkUndone clears done_at. In the same statement, it stamps
// pushed_at = now exactly when notified_at IS NOT NULL AND pushed_at IS
// NULL (§4.1). If a row is cleared during a cooldown, done_at drops it from
// the held set, and a later un-clear must not put it back into a digest. A
// row cleared before it ever fired keeps its NULLs, and returns as a live
// phase-1 candidate. On a row that is not done, it is a successful no-op.
// It returns sql.ErrNoRows if the id does not exist.
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

// DeleteReminder hard-deletes the row (§8.2: an endpoint for curl only). It
// reports whether a row existed.
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
// ordered comparison. The store owns this invariant: its own comparison in
// UpdateReminder depends on it, whichever caller writes.
func canonicalChannelList(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	canonical := slices.Clone(names)
	slices.Sort(canonical)
	return slices.Compact(canonical)
}

// encodeChannels stores an empty list as NULL: §4's column is nullable, and
// §8.2 turns NULL back into [] in the response.
func encodeChannels(names []string) *string {
	if len(names) == 0 {
		return nil
	}
	raw, _ := json.Marshal(names)
	s := string(raw)
	return &s
}

// decodeChannels is the inverse of encodeChannels, shared by every scanner
// of the column.
func decodeChannels(id int64, raw string) ([]string, error) {
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, fmt.Errorf("reminder %d: extra_channels: %w", id, err)
	}
	return names, nil
}
