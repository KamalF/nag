// Package store owns the SQLite database: opening it with the pinned
// connection settings (§4.3) and migrating it to the current schema (§4.2).
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// migrations is the ordered list of schema versions (§4.2); PRAGMA
// user_version counts how many have run. Append only — editing a shipped
// entry changes DDL that existing databases will never re-run.
var migrations = []string{
	// 1 — initial schema (§4)
	`
CREATE TABLE reminders (
  id             INTEGER PRIMARY KEY,
  text           TEXT    NOT NULL,        -- may contain [label](https://…) links, see §9.10
  due_at         INTEGER NOT NULL,
  notified_at    INTEGER,                 -- NULL = the sweep has not handled it yet
  pushed_at      INTEGER,                 -- NULL = handled, but not yet carried by a push (§7.3)
  done_at        INTEGER,                 -- NULL = still in the list
  created_at     INTEGER NOT NULL,
  extra_channels TEXT,                    -- JSON array of channel names, may be NULL
  delivery_error TEXT                     -- last fan-out failure, classified shape only (§4.1)
);
CREATE INDEX idx_reminders_pending ON reminders(due_at) WHERE done_at IS NULL;

CREATE TABLE push_subscriptions (
  id            INTEGER PRIMARY KEY,
  endpoint      TEXT NOT NULL UNIQUE,
  p256dh        TEXT NOT NULL,
  auth          TEXT NOT NULL,
  vapid_public  TEXT NOT NULL,            -- key this sub was created under, see §7.1
  created_at    INTEGER NOT NULL
);

CREATE TABLE channels (
  id      INTEGER PRIMARY KEY,
  name    TEXT NOT NULL UNIQUE,           -- lowercase slug, see §7.4; used in extra_channels
  url     TEXT NOT NULL,                  -- shoutrrr URL, CONTAINS SECRETS
  enabled INTEGER NOT NULL DEFAULT 1
);
`,
}

// Store is the process's single handle on the database. Every subcommand
// that touches NAG_DB goes through Open, so first touch always migrates.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite file at path — creating and migrating it if absent —
// and returns a handle pinned to one connection (§4.3). It refuses, naming
// the path, when the file cannot be opened or migrated, and refuses when the
// database's schema version is newer than this binary knows (§4.2).
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil { // sql.Open is lazy; open the file now
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	var have int
	if err := db.QueryRow("PRAGMA user_version").Scan(&have); err != nil {
		return err
	}
	if have > len(migrations) {
		return fmt.Errorf("database schema version is %d but this binary only knows %d: refusing to run an old binary against a newer database",
			have, len(migrations))
	}
	for i := have; i < len(migrations); i++ {
		if err := runMigration(db, i); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

// runMigration applies migrations[i] and bumps user_version to i+1 in the
// same transaction, so a failed migration leaves the version untouched.
func runMigration(db *sql.DB, i int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(migrations[i]); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
		return err
	}
	return tx.Commit()
}
