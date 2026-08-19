package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// CreateReminder inserts a reminder. When dueAt is not after now, the same
// statement stamps notified_at = pushed_at = now (§4.1): a backdated write
// appears in the overdue list immediately, is invisible to the sweep's
// phase 1, and can never become a digest candidate.
func (s *Store) CreateReminder(ctx context.Context, text string, dueAt int64, extraChannels []string, now int64) (Reminder, error) {
	var notified, pushed *int64
	if dueAt <= now {
		notified, pushed = &now, &now
	}
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

// ChannelNames returns every channel name mapped to its enabled flag —
// §8.3 accepts a disabled channel at write time (it is skipped at send
// time), so validation needs existence, not state.
func (s *Store) ChannelNames(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, enabled FROM channels")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		var enabled bool
		if err := rows.Scan(&name, &enabled); err != nil {
			return nil, err
		}
		names[name] = enabled
	}
	return names, rows.Err()
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
		if err := json.Unmarshal([]byte(channels.String), &r.ExtraChannels); err != nil {
			return Reminder{}, fmt.Errorf("reminder %d: extra_channels: %w", r.ID, err)
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
