package config_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KamalF/nag/internal/config"
	"github.com/KamalF/nag/internal/presets"
)

// The §5.5 hash tests. The failure mode is silent in both
// directions, so each test reads the running config and the counter.

const liveTOML = `[general]
timezone       = "Europe/Paris"
default_preset = "soon"
retention_days = 30

[picker]
hour_min     = 8
hour_max     = 18
minute_step  = 15
default_time = "09:00"
week_start   = "monday"

[[preset]]
key    = "soon"
label  = "Soon"
kind   = "offset"
offset = "30m"
quick  = true
`

type liveFixture struct {
	t    *testing.T
	path string
	live *config.Live
	logs *bytes.Buffer
	log  *slog.Logger
}

func newLive(t *testing.T) *liveFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nag.toml")
	writeFile(t, path, liveTOML)
	cfg, err := config.Check(path)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return &liveFixture{t: t, path: path, live: config.NewLive(path, cfg, "vapid"), logs: logs,
		log: slog.New(slog.NewTextHandler(logs, nil))}
}

func (f *liveFixture) reloadWith(content string) {
	f.t.Helper()
	writeFile(f.t, f.path, content)
	f.logs.Reset()
	f.live.Reload(f.log)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An offset-only edit is applied: the next tap of that chip lands at the new
// duration. But config_version does not move: the client cannot see offset,
// so nothing that it holds is stale.
func TestReloadOffsetOnlyAppliesWithoutMovingTheCounter(t *testing.T) {
	f := newLive(t)
	f.reloadWith(strings.Replace(liveTOML, `offset = "30m"`, `offset = "45m"`, 1))

	snap := f.live.Load()
	if snap.Version != 1 {
		t.Errorf("config_version = %d after an offset-only edit, want 1", snap.Version)
	}
	now := time.Unix(1786742400, 0)
	due, err := presets.Evaluate(snap.Presets[0], now, snap.Location)
	if err != nil {
		t.Fatal(err)
	}
	if got := due.Sub(now); got != 45*time.Minute {
		t.Errorf("chip resolves %v ahead, want the edited 45m", got)
	}
	if !strings.Contains(f.logs.String(), "config reloaded") {
		t.Errorf("an applied edit logged %q", f.logs.String())
	}
}

// A label rename is visible to the client, so the counter moves.
func TestReloadLabelRenameMovesTheCounter(t *testing.T) {
	f := newLive(t)
	f.reloadWith(strings.Replace(liveTOML, `label  = "Soon"`, `label  = "In a bit"`, 1))
	if v := f.live.Load().Version; v != 2 {
		t.Errorf("config_version = %d after a label rename, want 2", v)
	}
}

// A byte-identical file moves nothing and logs "config unchanged". This
// case is common: a second HUP after one edit reloads an unchanged file.
func TestReloadIdenticalFileChangesNothing(t *testing.T) {
	f := newLive(t)
	f.reloadWith(liveTOML)
	if v := f.live.Load().Version; v != 1 {
		t.Errorf("config_version = %d after an identical reload, want 1", v)
	}
	if !strings.Contains(f.logs.String(), "config unchanged") {
		t.Errorf("identical reload logged %q", f.logs.String())
	}
}

// A reload that fails validation, or finds the file gone, leaves both the
// running config and the counter untouched, and says so in the log.
func TestReloadFailureKeepsTheRunningConfig(t *testing.T) {
	for name, edit := range map[string]func(f *liveFixture){
		"invalid": func(f *liveFixture) {
			f.reloadWith(strings.Replace(liveTOML, `kind   = "offset"`, `kind   = "sometime"`, 1))
		},
		"missing": func(f *liveFixture) {
			if err := os.Remove(f.path); err != nil {
				f.t.Fatal(err)
			}
			f.live.Reload(f.log)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLive(t)
			before := f.live.Load()
			edit(f)
			if after := f.live.Load(); after != before {
				t.Error("a failed reload replaced the running config")
			}
			if !strings.Contains(f.logs.String(), "keeping the running config") {
				t.Errorf("failure not logged: %q", f.logs.String())
			}
			if _, err := os.Stat(f.path); name == "missing" && err == nil {
				t.Error("a reload recreated the missing file")
			}
		})
	}
}
