package presets

import (
	"testing"
	"time"

	"github.com/KamalF/nag/internal/config"
)

// Europe/Paris in 2026: spring forward Sun Mar 29 02:00→03:00 (+01:00 to
// +02:00), fall back Sun Oct 25 03:00→02:00.
var paris = mustLoad("Europe/Paris")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func ptr[T any](v T) *T { return &v }

func offsetPreset(offset string) config.Preset {
	return config.Preset{Key: "t", Label: "T", Kind: "offset", Offset: ptr(offset)}
}

func clockPreset(at string, days int) config.Preset {
	return config.Preset{Key: "t", Label: "T", Kind: "clock", At: ptr(at), Days: ptr(days)}
}

func weekdayPreset(weekday, at string, sameDayOK bool) config.Preset {
	return config.Preset{Key: "t", Label: "T", Kind: "weekday",
		Weekday: ptr(weekday), At: ptr(at), SameDayOK: ptr(sameDayOK)}
}

func evaluate(t *testing.T, p config.Preset, now time.Time) time.Time {
	t.Helper()
	due, err := Evaluate(p, now, paris)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return due
}

func assertLocal(t *testing.T, due time.Time, want string) {
	t.Helper()
	if got := due.In(paris).Format("2006-01-02 15:04 -0700"); got != want {
		t.Errorf("due = %s, want %s", got, want)
	}
}

func TestClockAcrossSpringForward(t *testing.T) {
	// Sat Mar 28 10:00 CET → "tomorrow 09:00" must be 09:00 CEST: 22 real
	// hours away, not now+24h (which would read 10:00).
	now := time.Date(2026, 3, 28, 10, 0, 0, 0, paris)
	due := evaluate(t, clockPreset("09:00", 1), now)
	assertLocal(t, due, "2026-03-29 09:00 +0200")
	if elapsed := due.Sub(now); elapsed != 22*time.Hour {
		t.Errorf("elapsed = %v, want 22h (calendar day across the transition)", elapsed)
	}
}

func TestClockAcrossFallBack(t *testing.T) {
	// Sat Oct 24 10:00 CEST → Sun Oct 25 09:00 CET: 23 clock-face hours
	// plus the repeated hour = 24 real hours.
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, paris)
	due := evaluate(t, clockPreset("09:00", 1), now)
	assertLocal(t, due, "2026-10-25 09:00 +0100")
	if elapsed := due.Sub(now); elapsed != 24*time.Hour {
		t.Errorf("elapsed = %v, want 24h", elapsed)
	}
}

func TestClockAtInsideSpringForwardGap(t *testing.T) {
	// 02:30 does not exist on Mar 29 — time.Date normalises it forward to
	// 03:30, and §6 says to accept exactly that, not detect the gap.
	now := time.Date(2026, 3, 28, 10, 0, 0, 0, paris)
	due := evaluate(t, clockPreset("02:30", 1), now)
	assertLocal(t, due, "2026-03-29 03:30 +0200")
}

func TestWeekdayOnItsOwnDay(t *testing.T) {
	saturday := time.Date(2026, 8, 22, 8, 0, 0, 0, paris) // a Saturday

	t.Run("same_day_ok true, at still ahead → today", func(t *testing.T) {
		due := evaluate(t, weekdayPreset("saturday", "09:00", true), saturday)
		assertLocal(t, due, "2026-08-22 09:00 +0200")
	})
	t.Run("same_day_ok true, at already past → +7", func(t *testing.T) {
		late := time.Date(2026, 8, 22, 10, 0, 0, 0, paris)
		due := evaluate(t, weekdayPreset("saturday", "09:00", true), late)
		assertLocal(t, due, "2026-08-29 09:00 +0200")
	})
	t.Run("same_day_ok false → +7 even with at ahead", func(t *testing.T) {
		due := evaluate(t, weekdayPreset("saturday", "09:00", false), saturday)
		assertLocal(t, due, "2026-08-29 09:00 +0200")
	})
}

func TestWeekdayLaterThisWeek(t *testing.T) {
	tuesday := time.Date(2026, 8, 18, 8, 0, 0, 0, paris) // a Tuesday
	due := evaluate(t, weekdayPreset("friday", "14:00", false), tuesday)
	assertLocal(t, due, "2026-08-21 14:00 +0200")
}

func TestOffsetAcrossMidnight(t *testing.T) {
	now := time.Date(2026, 8, 19, 23, 50, 0, 0, paris)
	due := evaluate(t, offsetPreset("30m"), now)
	assertLocal(t, due, "2026-08-20 00:20 +0200")
	if elapsed := due.Sub(now); elapsed != 30*time.Minute {
		t.Errorf("elapsed = %v, want exactly 30m", elapsed)
	}
}

func TestOffsetAcrossSpringForwardIsPureDuration(t *testing.T) {
	// 00:30 CET + 3h of wall time lands at 04:30 CEST — the calendar never
	// enters into an offset (§6).
	now := time.Date(2026, 3, 29, 0, 30, 0, 0, paris)
	due := evaluate(t, offsetPreset("3h"), now)
	assertLocal(t, due, "2026-03-29 04:30 +0200")
	if elapsed := due.Sub(now); elapsed != 3*time.Hour {
		t.Errorf("elapsed = %v, want exactly 3h", elapsed)
	}
}

func TestClockAt2350(t *testing.T) {
	now := time.Date(2026, 8, 19, 23, 50, 0, 0, paris)
	due := evaluate(t, clockPreset("09:00", 0), now)
	assertLocal(t, due, "2026-08-20 09:00 +0200")
}

func TestClockDaysZeroBothSidesOfAt(t *testing.T) {
	t.Run("before at → today", func(t *testing.T) {
		now := time.Date(2026, 8, 19, 8, 0, 0, 0, paris)
		due := evaluate(t, clockPreset("09:00", 0), now)
		assertLocal(t, due, "2026-08-19 09:00 +0200")
	})
	t.Run("after at → tomorrow", func(t *testing.T) {
		now := time.Date(2026, 8, 19, 10, 0, 0, 0, paris)
		due := evaluate(t, clockPreset("09:00", 0), now)
		assertLocal(t, due, "2026-08-20 09:00 +0200")
	})
	t.Run("exactly at → tomorrow (not after now)", func(t *testing.T) {
		now := time.Date(2026, 8, 19, 9, 0, 0, 0, paris)
		due := evaluate(t, clockPreset("09:00", 0), now)
		assertLocal(t, due, "2026-08-20 09:00 +0200")
	})
}

func TestEvaluateReturnsUTC(t *testing.T) {
	now := time.Date(2026, 8, 19, 8, 0, 0, 0, paris)
	due := evaluate(t, clockPreset("09:00", 0), now)
	if due.Location() != time.UTC {
		t.Errorf("due location = %v, want UTC", due.Location())
	}
}
