package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCreatesAndMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if got := userVersion(t, s.db); got != len(migrations) {
		t.Errorf("user_version = %d, want %d", got, len(migrations))
	}
	for _, name := range []string{"reminders", "push_subscriptions", "channels"} {
		var n int
		err := s.db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name,
		).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s: found %d (err %v), want 1", name, n, err)
		}
	}
	var n int
	err = s.db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_reminders_pending'",
	).Scan(&n)
	if err != nil || n != 1 {
		t.Errorf("index idx_reminders_pending: found %d (err %v), want 1", n, err)
	}
}

func TestReopenIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s.db.Exec(
		"INSERT INTO reminders (text, due_at, created_at) VALUES ('x', 1, 1)",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM reminders").Scan(&n); err != nil || n != 1 {
		t.Errorf("reminders after reopen: %d (err %v), want 1", n, err)
	}
	if got := userVersion(t, s.db); got != len(migrations) {
		t.Errorf("user_version after reopen = %d, want %d", got, len(migrations))
	}
}

func TestNewerDatabaseRefusesNamingBothNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	s.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("Open succeeded against a newer database, want refusal")
	}
	for _, want := range []string{"99", "1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestUnopenableFileRefusesNamingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "nag.db")
	_, err := Open(path)
	if err == nil {
		t.Fatal("Open succeeded on an uncreatable path, want refusal")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %q", err, path)
	}
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}
