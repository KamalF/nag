package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAbsentPathWritesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.toml")
	cfg, wrote, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !wrote {
		t.Error("wroteDefault = false, want true on a genuinely missing path")
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("default file not written: %v", err)
	}
	if string(written) != string(defaultTOML) {
		t.Error("written file differs from the embedded default")
	}

	if cfg.General.Timezone != "Europe/Paris" {
		t.Errorf("timezone = %q, want Europe/Paris", cfg.General.Timezone)
	}
	if cfg.General.DefaultPreset != "tomorrow" {
		t.Errorf("default_preset = %q, want tomorrow", cfg.General.DefaultPreset)
	}
	if cfg.General.RetentionDays != 30 {
		t.Errorf("retention_days = %d, want 30", cfg.General.RetentionDays)
	}
	if cfg.Picker.MinuteStep != 15 || cfg.Picker.WeekStart != "monday" {
		t.Errorf("picker = %+v, want minute_step 15, week_start monday", cfg.Picker)
	}
	if len(cfg.Presets) != 4 {
		t.Fatalf("presets = %d, want 4", len(cfg.Presets))
	}
}

// Presence of a kind-specific field must survive decoding: §5.4 rejects a
// field outside its kind, so nil vs set is meaning, not representation.
func TestLoadKeepsFieldPresence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.toml")
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	offset := cfg.Presets[0] // 30min
	if offset.Offset == nil || *offset.Offset != "30m" {
		t.Errorf("offset preset: Offset = %v, want 30m set", offset.Offset)
	}
	if offset.At != nil || offset.Days != nil || offset.Weekday != nil || offset.SameDayOK != nil {
		t.Errorf("offset preset carries absent fields as set: %+v", offset)
	}
	clock := cfg.Presets[2] // tomorrow
	if clock.At == nil || *clock.At != "09:00" || clock.Days == nil || *clock.Days != 1 {
		t.Errorf("clock preset: At = %v, Days = %v, want 09:00 and 1 set", clock.At, clock.Days)
	}
	weekday := cfg.Presets[3] // next-monday
	if weekday.SameDayOK == nil || *weekday.SameDayOK != false {
		t.Errorf("weekday preset: SameDayOK = %v, want false set (present in the file)", weekday.SameDayOK)
	}
}

func TestLoadExistingFileIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.toml")
	own := `[general]
timezone = "UTC"
default_preset = "soon"
retention_days = 0

[picker]
hour_min = 0
hour_max = 23
minute_step = 5
default_time = "10:00"
week_start = "sunday"

[[preset]]
key = "soon"
label = "Soon"
kind = "offset"
offset = "10m"
`
	if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, wrote, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wrote {
		t.Error("wroteDefault = true on a present file")
	}
	if cfg.General.Timezone != "UTC" {
		t.Errorf("timezone = %q, want the file's UTC, not the default", cfg.General.Timezone)
	}
}

func TestLoadBrokenFileRefusesAndPreserves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.toml")
	broken := "[general\ntimezone ="
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded on a broken file, want refusal")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != broken {
		t.Error("broken file was overwritten — the default must only land on a missing path")
	}
}

func TestLoadUnreadableFileRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	path := filepath.Join(t.TempDir(), "nag.toml")
	if err := os.WriteFile(path, defaultTOML, 0o000); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded on an unreadable file, want refusal")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path", err)
	}
}

func TestLoadMissingDirectoryRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "nag.toml")
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded with an unwritable config directory, want refusal")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path", err)
	}
}
