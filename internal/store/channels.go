package store

import (
	"context"
	"database/sql"
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrChannelExists and ErrNoSuchChannel are the two answers that
// `nag channel` turns into a sentence (§7.4).
var (
	ErrChannelExists = errors.New("channel already exists")
	ErrNoSuchChannel = errors.New("no such channel")
)

// ChannelRow is a whole channels row, URL included. Only the CLI and the
// sweep's send path read it: the URL carries secrets and never reaches the
// API in any form (§4.1).
type ChannelRow struct {
	Name    string
	URL     string
	Enabled bool
}

// AddChannel inserts an enabled channel. The name's UNIQUE constraint
// rejects a duplicate. The caller checks the slug and parses the URL first
// (§7.4).
func (s *Store) AddChannel(ctx context.Context, name, url string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO channels (name, url, enabled) VALUES (?, ?, 1)", name, url)
	var e *sqlite.Error
	if errors.As(err, &e) && e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return ErrChannelExists
	}
	return err
}

// IsBusy reports whether err means that the database stayed locked until
// busy_timeout ran out. The cause is the serving container, which holds the
// write lock while `nag channel` runs against the same file (§4.3, §7.4).
// The primary result code is the low byte of the extended code.
func IsBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_BUSY
}

// ChannelRows returns every channel with its URL, in name order.
func (s *Store) ChannelRows(ctx context.Context) ([]ChannelRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, url, enabled FROM channels ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRow
	for rows.Next() {
		var c ChannelRow
		if err := rows.Scan(&c.Name, &c.URL, &c.Enabled); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChannelByName returns one channel with its URL — for `nag channel test`,
// which sends to a disabled channel too.
func (s *Store) ChannelByName(ctx context.Context, name string) (ChannelRow, error) {
	c := ChannelRow{Name: name}
	err := s.db.QueryRowContext(ctx, "SELECT url, enabled FROM channels WHERE name = ?", name).
		Scan(&c.URL, &c.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNoSuchChannel
	}
	return c, err
}

// ChannelReferences counts the reminders whose extra_channels name the
// channel, cleared ones included: an undo can bring any of them back.
func (s *Store) ChannelReferences(ctx context.Context, name string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reminders, json_each(reminders.extra_channels)
		WHERE reminders.extra_channels IS NOT NULL AND json_each.value = ?`, name).Scan(&n)
	return n, err
}

// RemoveChannel deletes the channel row. Reminders that still name the
// channel become orphans, which is defined behavior (§4.1): the name is
// skipped at send time and kept on the reminder.
func (s *Store) RemoveChannel(ctx context.Context, name string) error {
	return s.affectOne(ctx, "DELETE FROM channels WHERE name = ?", name)
}

// SetChannelEnabled mutes or unmutes a channel without orphaning anything.
func (s *Store) SetChannelEnabled(ctx context.Context, name string, enabled bool) error {
	return s.affectOne(ctx, "UPDATE channels SET enabled = ? WHERE name = ?", enabled, name)
}

func (s *Store) affectOne(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoSuchChannel
	}
	return nil
}
