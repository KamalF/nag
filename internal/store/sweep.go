package store

import (
	"context"
	"database/sql"
	"strings"
)

// MarkedRow is phase 1's hand-off: the tick fans out exactly what it just
// marked and never re-queries, so the message text has to come out of
// this SELECT (§7.3).
type MarkedRow struct {
	ID            int64
	Text          string
	DueAt         int64
	ExtraChannels []string
}

// SweepMark is §7.3 phase 1, one transaction, writes only: select up to 50
// due unmarked rows, stamp notified_at = now on all of them, and stamp
// pushed_at = now too on those past the too-late gate — rows due more than
// tooLateAfter seconds ago (the notify package owns that policy number).
// It returns the eligible rows for the later phases — gate-stamped rows
// excluded: the gate means no output at all — and the gated ids for the
// INFO line.
func (s *Store) SweepMark(ctx context.Context, now, tooLateAfter int64) (eligible []MarkedRow, tooLate []int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, text, due_at, extra_channels FROM reminders
		WHERE done_at IS NULL AND notified_at IS NULL AND due_at <= ?
		ORDER BY due_at, id LIMIT 50`, now)
	if err != nil {
		return nil, nil, err
	}
	var all []MarkedRow
	for rows.Next() {
		var r MarkedRow
		var channels *string
		if err := rows.Scan(&r.ID, &r.Text, &r.DueAt, &channels); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if channels != nil {
			if r.ExtraChannels, err = decodeChannels(r.ID, *channels); err != nil {
				rows.Close()
				return nil, nil, err
			}
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(all) == 0 {
		return nil, nil, tx.Commit()
	}

	var allIDs []int64
	for _, r := range all {
		allIDs = append(allIDs, r.ID)
		if r.DueAt < now-tooLateAfter {
			tooLate = append(tooLate, r.ID)
		} else {
			eligible = append(eligible, r)
		}
	}
	if err := stampByID(ctx, tx, "notified_at", now, allIDs); err != nil {
		return nil, nil, err
	}
	if err := stampByID(ctx, tx, "pushed_at", now, tooLate); err != nil {
		return nil, nil, err
	}
	return eligible, tooLate, tx.Commit()
}

func stampByID(ctx context.Context, tx *sql.Tx, column string, now int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := tx.ExecContext(ctx,
		"UPDATE reminders SET "+column+" = ? WHERE id IN ("+placeholders+")", args...)
	return err
}
