package config

import (
	"fmt"
	"time"
)

// Weekdays is the closed name set for preset `weekday` fields and
// `[picker].week_start` (§5.4, §5.3).
var Weekdays = map[string]time.Weekday{
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
	"sunday":    time.Sunday,
}

// validate applies the §5.5 rule list and the §5.4 per-kind field matrix.
// It returns the first violation found, worded to name the offending preset
// key (or [section].field) and the rejected value — the file is hand-edited
// under SIGHUP, so a located error is the whole point (§5.5).
func validate(cfg *Config) error {
	if err := validateGeneral(cfg.General); err != nil {
		return err
	}
	if err := validatePicker(cfg.Picker); err != nil {
		return err
	}
	if len(cfg.Presets) == 0 {
		return fmt.Errorf("no [[preset]] entries: the preset list must not be empty")
	}
	seen := make(map[string]bool, len(cfg.Presets))
	for _, p := range cfg.Presets {
		if err := validatePreset(p); err != nil {
			return err
		}
		if seen[p.Key] {
			return fmt.Errorf("preset %q: duplicate key", p.Key)
		}
		seen[p.Key] = true
	}
	if !seen[cfg.General.DefaultPreset] {
		return fmt.Errorf("[general].default_preset: %q does not match any preset key",
			cfg.General.DefaultPreset)
	}
	return nil
}

func validateGeneral(g General) error {
	// LoadLocation("") is UTC and "Local" is wherever the process runs —
	// both nil-error, neither a timezone the file actually states, and a
	// missing key must fail loudly (§5.5), not schedule in UTC silently.
	if g.Timezone == "" || g.Timezone == "Local" {
		return fmt.Errorf("[general].timezone: %q is not a timezone name (e.g. \"Europe/Paris\")", g.Timezone)
	}
	if _, err := time.LoadLocation(g.Timezone); err != nil {
		return fmt.Errorf("[general].timezone: unknown timezone %q", g.Timezone)
	}
	if g.RetentionDays < 0 {
		return fmt.Errorf("[general].retention_days: %d is negative (0 = keep forever)",
			g.RetentionDays)
	}
	return nil
}

func validatePicker(p Picker) error {
	if p.HourMin < 0 || p.HourMin > 23 {
		return fmt.Errorf("[picker].hour_min: %d is outside 0..23", p.HourMin)
	}
	if p.HourMax < 0 || p.HourMax > 23 {
		return fmt.Errorf("[picker].hour_max: %d is outside 0..23", p.HourMax)
	}
	if p.HourMin > p.HourMax {
		return fmt.Errorf("[picker].hour_min: %d is greater than hour_max %d",
			p.HourMin, p.HourMax)
	}
	if p.MinuteStep < 1 || p.MinuteStep > 60 || 60%p.MinuteStep != 0 {
		return fmt.Errorf("[picker].minute_step: %d must be in 1..60 and divide 60",
			p.MinuteStep)
	}
	if p.WeekStart != "monday" && p.WeekStart != "sunday" {
		return fmt.Errorf("[picker].week_start: %q is not \"monday\" or \"sunday\"",
			p.WeekStart)
	}
	h, m, err := ParseHHMM(p.DefaultTime)
	if err != nil {
		return fmt.Errorf("[picker].default_time: %q is not HH:MM in 24-hour form",
			p.DefaultTime)
	}
	if h < p.HourMin || h > p.HourMax {
		return fmt.Errorf("[picker].default_time: %q is outside hours %d..%d",
			p.DefaultTime, p.HourMin, p.HourMax)
	}
	if m%p.MinuteStep != 0 {
		return fmt.Errorf("[picker].default_time: %q is not on a %d-minute boundary",
			p.DefaultTime, p.MinuteStep)
	}
	return nil
}

// validatePreset enforces the §5.4 matrix: each kind has an exact field
// set, and a field belonging to another kind is an error, not noise.
func validatePreset(p Preset) error {
	if p.Key == "" {
		return fmt.Errorf("preset with label %q: empty key", p.Label)
	}
	if p.Label == "" {
		return fmt.Errorf("preset %q: empty label", p.Key)
	}
	switch p.Kind {
	case "offset":
		if name := firstSet(
			field{"at", p.At != nil},
			field{"days", p.Days != nil},
			field{"weekday", p.Weekday != nil},
			field{"same_day_ok", p.SameDayOK != nil},
		); name != "" {
			return rejectedFieldError(p.Key, p.Kind, name)
		}
		if p.Offset == nil {
			return fmt.Errorf("preset %q: kind \"offset\" requires an offset field", p.Key)
		}
		d, err := time.ParseDuration(*p.Offset)
		if err != nil {
			return fmt.Errorf("preset %q: unparseable offset %q", p.Key, *p.Offset)
		}
		if d <= 0 {
			return fmt.Errorf("preset %q: offset %q must be positive", p.Key, *p.Offset)
		}
	case "clock":
		if name := firstSet(
			field{"offset", p.Offset != nil},
			field{"weekday", p.Weekday != nil},
			field{"same_day_ok", p.SameDayOK != nil},
		); name != "" {
			return rejectedFieldError(p.Key, p.Kind, name)
		}
		if p.At == nil {
			return fmt.Errorf("preset %q: kind \"clock\" requires an at field", p.Key)
		}
		if _, _, err := ParseHHMM(*p.At); err != nil {
			return fmt.Errorf("preset %q: at %q is not HH:MM in 24-hour form", p.Key, *p.At)
		}
		if p.Days != nil && *p.Days < 0 {
			return fmt.Errorf("preset %q: days %d is negative", p.Key, *p.Days)
		}
	case "weekday":
		if name := firstSet(
			field{"offset", p.Offset != nil},
			field{"days", p.Days != nil},
		); name != "" {
			return rejectedFieldError(p.Key, p.Kind, name)
		}
		if p.Weekday == nil {
			return fmt.Errorf("preset %q: kind \"weekday\" requires a weekday field", p.Key)
		}
		if _, ok := Weekdays[*p.Weekday]; !ok {
			return fmt.Errorf("preset %q: unknown weekday %q", p.Key, *p.Weekday)
		}
		if p.At == nil {
			return fmt.Errorf("preset %q: kind \"weekday\" requires an at field", p.Key)
		}
		if _, _, err := ParseHHMM(*p.At); err != nil {
			return fmt.Errorf("preset %q: at %q is not HH:MM in 24-hour form", p.Key, *p.At)
		}
	default:
		return fmt.Errorf("preset %q: unknown kind %q (offset | clock | weekday)",
			p.Key, p.Kind)
	}
	return nil
}

type field struct {
	name string
	set  bool
}

func firstSet(fields ...field) string {
	for _, f := range fields {
		if f.set {
			return f.name
		}
	}
	return ""
}

func rejectedFieldError(key, kind, name string) error {
	return fmt.Errorf("preset %q: field %q does not belong to kind %q", key, name, kind)
}

// ParseHHMM parses a strict 24-hour "HH:MM" — exactly five characters,
// zero-padded, 00:00 to 23:59.
func ParseHHMM(s string) (hour, minute int, err error) {
	bad := fmt.Errorf("%q is not HH:MM", s)
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, bad
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, 0, bad
		}
	}
	hour = int(s[0]-'0')*10 + int(s[1]-'0')
	minute = int(s[3]-'0')*10 + int(s[4]-'0')
	if hour > 23 || minute > 59 {
		return 0, 0, bad
	}
	return hour, minute, nil
}
