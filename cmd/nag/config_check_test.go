package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `[general]
timezone = "Europe/Paris"
default_preset = "p"
retention_days = 30

[picker]
hour_min = 8
hour_max = 18
minute_step = 15
default_time = "09:00"
week_start = "monday"

[[preset]]
key = "z-first"
label = "Z first"
kind = "offset"
offset = "30m"

[[preset]]
key = "p"
label = "P"
kind = "clock"
at = "09:00"
days = 1
`

func TestConfigCheckValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.toml")
	if err := os.WriteFile(path, []byte(validConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NAG_CONFIG", path)

	var stdout, stderr strings.Builder
	if got := run([]string{"config", "check"}, &stdout, &stderr); got != 0 {
		t.Fatalf("exit = %d (stderr %q), want 0", got, stderr.String())
	}
	out := stdout.String()
	zPos, pPos := strings.Index(out, "z-first"), strings.Index(out, "p ")
	if zPos < 0 || pPos < 0 || zPos > pPos {
		t.Errorf("preset list not in file order:\n%s", out)
	}
}

func TestConfigCheckInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.toml")
	broken := strings.Replace(validConfig, `offset = "30m"`, `offset = "-30m"`, 1)
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NAG_CONFIG", path)

	var stdout, stderr strings.Builder
	if got := run([]string{"config", "check"}, &stdout, &stderr); got != 1 {
		t.Fatalf("exit = %d, want 1", got)
	}
	errText := stderr.String()
	for _, want := range []string{"z-first", "-30m"} {
		if !strings.Contains(errText, want) {
			t.Errorf("stderr %q does not locate the error (%q missing)", errText, want)
		}
	}
}

func TestConfigCheckAbsentFileIsAnErrorAndWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nag.toml")
	t.Setenv("NAG_CONFIG", path)

	var stdout, stderr strings.Builder
	if got := run([]string{"config", "check"}, &stdout, &stderr); got != 1 {
		t.Fatalf("exit = %d, want 1", got)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("config check wrote the default file — it must never fix silently")
	}
}

func TestConfigWithoutCheckIsUsage(t *testing.T) {
	for _, args := range [][]string{{"config"}, {"config", "validate"}} {
		var stdout, stderr strings.Builder
		if got := run(args, &stdout, &stderr); got != 2 {
			t.Errorf("run(%v) = %d, want 2", args, got)
		}
	}
}
