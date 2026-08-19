// Package presets resolves a preset into a due instant (§6). All arithmetic
// happens in the server's general.timezone, then converts to UTC; times are
// built with time.Date so the stdlib handles DST transitions (a wall-clock
// time inside a spring-forward gap normalises forward, by design).
package presets

import (
	"fmt"
	"time"

	"github.com/KamalF/nag/internal/config"
)

// Evaluate resolves preset p at instant now, in loc. p must have passed
// config validation (§5.5) — an unvalidated shape is an error, never a
// guess.
func Evaluate(p config.Preset, now time.Time, loc *time.Location) (time.Time, error) {
	switch p.Kind {
	case "offset":
		return evaluateOffset(p, now)
	case "clock":
		return evaluateClock(p, now, loc)
	case "weekday":
		return evaluateWeekday(p, now, loc)
	default:
		return time.Time{}, fmt.Errorf("preset %q: unknown kind %q", p.Key, p.Kind)
	}
}

// evaluateOffset is pure duration arithmetic on an absolute instant — it
// never touches a calendar, so "3 hours" is three hours of wall time even
// across a DST transition (§6).
func evaluateOffset(p config.Preset, now time.Time) (time.Time, error) {
	if p.Offset == nil {
		return time.Time{}, fmt.Errorf("preset %q: no offset", p.Key)
	}
	d, err := time.ParseDuration(*p.Offset)
	if err != nil {
		return time.Time{}, fmt.Errorf("preset %q: offset %q: %w", p.Key, *p.Offset, err)
	}
	return now.Add(d).UTC(), nil
}

// evaluateClock advances today's date parts by `days` calendar days — never
// the instant by days*24h — and builds the result at `at` in loc. A result
// not after now advances one more calendar day and rebuilds (§6).
func evaluateClock(p config.Preset, now time.Time, loc *time.Location) (time.Time, error) {
	if p.At == nil {
		return time.Time{}, fmt.Errorf("preset %q: no at", p.Key)
	}
	hour, minute, err := config.ParseHHMM(*p.At)
	if err != nil {
		return time.Time{}, fmt.Errorf("preset %q: %w", p.Key, err)
	}
	days := 0
	if p.Days != nil {
		days = *p.Days
	}
	y, m, d := now.In(loc).Date()
	due := time.Date(y, m, d+days, hour, minute, 0, 0, loc)
	if !due.After(now) {
		due = time.Date(y, m, d+days+1, hour, minute, 0, 0, loc)
	}
	return due.UTC(), nil
}

// evaluateWeekday finds the next date whose weekday matches, at `at`. Today
// counts only when same_day_ok is set and `at` is still in the future;
// otherwise the match is strictly after today — on the target weekday
// itself, same_day_ok = false means +7 days (§6).
func evaluateWeekday(p config.Preset, now time.Time, loc *time.Location) (time.Time, error) {
	if p.Weekday == nil || p.At == nil {
		return time.Time{}, fmt.Errorf("preset %q: missing weekday or at", p.Key)
	}
	target, ok := config.Weekdays[*p.Weekday]
	if !ok {
		return time.Time{}, fmt.Errorf("preset %q: unknown weekday %q", p.Key, *p.Weekday)
	}
	hour, minute, err := config.ParseHHMM(*p.At)
	if err != nil {
		return time.Time{}, fmt.Errorf("preset %q: %w", p.Key, err)
	}
	local := now.In(loc)
	y, m, d := local.Date()
	delta := (int(target) - int(local.Weekday()) + 7) % 7
	if delta == 0 {
		if p.SameDayOK != nil && *p.SameDayOK {
			today := time.Date(y, m, d, hour, minute, 0, 0, loc)
			if today.After(now) {
				return today.UTC(), nil
			}
		}
		delta = 7
	}
	return time.Date(y, m, d+delta, hour, minute, 0, 0, loc).UTC(), nil
}
