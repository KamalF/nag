// Package config loads the TOML behaviour file (§5.3): types, the embedded
// default written out on first boot, and — in later commits — validation
// (§5.4, §5.5) and SIGHUP reload.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

//go:embed nag.default.toml
var defaultTOML []byte

type Config struct {
	General General  `toml:"general"`
	Picker  Picker   `toml:"picker"`
	Presets []Preset `toml:"preset"`
}

type General struct {
	Timezone      string `toml:"timezone"`
	DefaultPreset string `toml:"default_preset"`
	RetentionDays int    `toml:"retention_days"`
}

// Picker constrains only the picker sheet (§5.4) — never when reminders
// may fire.
type Picker struct {
	HourMin     int    `toml:"hour_min"`
	HourMax     int    `toml:"hour_max"`
	MinuteStep  int    `toml:"minute_step"`
	DefaultTime string `toml:"default_time"`
	WeekStart   string `toml:"week_start"`
}

// Preset carries the union of every kind's fields. The kind-specific ones
// are pointers because §5.4 makes *presence* meaningful: a field outside
// its kind's set is a config error, not noise, so nil ("absent from the
// file") and a set zero value must stay distinguishable.
type Preset struct {
	Key       string  `toml:"key"`
	Label     string  `toml:"label"`
	Kind      string  `toml:"kind"`
	Offset    *string `toml:"offset"`
	At        *string `toml:"at"`
	Days      *int    `toml:"days"`
	Weekday   *string `toml:"weekday"`
	SameDayOK *bool   `toml:"same_day_ok"`
	Quick     bool    `toml:"quick"`
}

// Load reads, decodes, and validates the TOML config at path. When the path
// is genuinely absent it first writes the embedded default there and reports
// wroteDefault = true. A file that is present but unreadable, unparseable,
// or invalid is an error naming the path — never overwritten with the
// default (§5.3).
func Load(path string) (cfg *Config, wroteDefault bool, err error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		if werr := os.WriteFile(path, defaultTOML, 0o644); werr != nil {
			return nil, false, fmt.Errorf("write default config to %s: %w", path, werr)
		}
		raw, wroteDefault = defaultTOML, true
	default:
		return nil, false, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err = parse(raw)
	if err != nil {
		return nil, wroteDefault, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, wroteDefault, nil
}

// Check loads and validates the config exactly as boot does, except that an
// absent file is an error rather than a prompt to write the default —
// `nag config check` never fixes anything silently (§5.5). The reload path
// (M4) reuses it for the same reason.
func Check(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // §5.5: an unknown key or section is an error
	if err := dec.Decode(&cfg); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return nil, unknownKeysError(strict)
		}
		return nil, err
	}
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// unknownKeysError names every key the file states that the app never reads
// (§5.5): silently ignoring one means a chip behaving as though the line
// were not there.
func unknownKeysError(strict *toml.StrictMissingError) error {
	names := make([]string, 0, len(strict.Errors))
	for _, e := range strict.Errors {
		names = append(names, strings.Join(e.Key(), "."))
	}
	return fmt.Errorf("unknown key or section: %s", strings.Join(names, ", "))
}
